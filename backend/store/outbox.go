package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aceaura/model-surge-agent/backend/relayclient"
	"github.com/aceaura/model-surge-agent/backend/textsafe"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("not found")

// ErrLeaseLost 表示这一行的所有权已不在调用方手里，裁决未生效。
//
// 不是故障：租约过期后被重新认领、或管理面点了重试按钮，都会走到这里。
// 与 ErrNotFound 分开是因为处置不同——丢租约意味着「有别人在管这一行」，
// 调用方该放手；找不到意味着运维给的 report_id 不对。
var ErrLeaseLost = errors.New("outbox lease lost")

// Outbox 是结果上报的持久队列。直报失败的上报存这里由 worker 重放，
// 因为调度层的冷却与用量依赖这些数字，丢了会让决策失真。
type Outbox struct {
	pool *pgxpool.Pool
}

func NewOutbox(pool *pgxpool.Pool) *Outbox { return &Outbox{pool: pool} }

// Entry 是队列中的一条待上报项。
type Entry struct {
	ID            int64
	ReportID      string
	Report        relayclient.ResultReport
	Attempts      int
	NextAttemptAt time.Time
	LastError     string
	CreatedAt     time.Time
	// LeaseToken 是认领这一行时拿到的所有权凭据，写回裁决时必须带上。
	// 观测用的 List 不认领，那里它是零值。
	LeaseToken [16]byte
}

// Enqueue 入队。report_id 冲突即丢弃：同键已在队列里，
// 调度层又按同键幂等去重，重复入队没有意义。
func (o *Outbox) Enqueue(ctx context.Context, rep relayclient.ResultReport, lastErr string) error {
	raw, err := json.Marshal(rep)
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	_, err = o.pool.Exec(ctx, `
		INSERT INTO report_outbox (report_id, report_json, last_error)
		VALUES ($1, $2, $3)
		ON CONFLICT (report_id) DO NOTHING`,
		rep.ReportID, raw, lastErr)
	return err
}

// defaultLease 是认领的默认租约长度，供 lease 为非正值时兜底。
const defaultLease = 30 * time.Second

// Due 原子地认领到期的待上报项。limit 为 0 时取 100 条。
//
// 认领而非只读：只读的话两个执行流会拿到同一批行，各把同一次失败记一笔，
// 于是 attempts 以双倍速度烧完，一条只是撞上调度层抖动的上报被提前判死，
// 而管理面显示的是「重试了 20 次仍失败」这个假结论。单进程里这两个执行流
// 一直都在——ticker 的 Drain 与关停期那次 Drain。
//
// 两道机制各管一段。FOR UPDATE SKIP LOCKED 管同一瞬间：后到的执行流跳过
// 被锁的行而不是排队等（排队等的结果是它拿到同一批行的旧快照）。lease_until
// 管跨瞬间：行锁随这条语句的事务结束就释放了，而处理是之后才做的一次真实
// HTTP 上报，最长十秒。
//
// 租约到期即重新可见，这是刻意的：持有者可能已经随进程被杀掉了，
// 那些行必须回到队列里，而不是永久隐身。
func (o *Outbox) Due(ctx context.Context, now time.Time, limit int, lease time.Duration) ([]Entry, error) {
	if limit <= 0 {
		limit = 100
	}
	if lease <= 0 {
		lease = defaultLease
	}
	rows, err := o.pool.Query(ctx, `
		WITH claimed AS (
			SELECT id FROM report_outbox
			WHERE next_attempt_at <= $1
			  AND (lease_until IS NULL OR lease_until <= $1)
			ORDER BY next_attempt_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		UPDATE report_outbox o
		SET lease_until = $1 + $3::interval, lease_token = gen_random_uuid()
		WHERE o.id IN (SELECT id FROM claimed)
		RETURNING o.id, o.report_id, o.report_json, o.attempts,
		          o.next_attempt_at, o.last_error, o.created_at, o.lease_token`,
		now, limit, lease)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Entry{}
	for rows.Next() {
		e, err := scanClaimed(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Done 上报成功，删行。token 认不上返回 ErrLeaseLost。
//
// 删行也要围栏：不围的话无法区分「另一个执行流已经把它干完了」与
// 「我的租约被抢了」，而这两种情形下运维该看到的日志不同。
func (o *Outbox) Done(ctx context.Context, id int64, token [16]byte) error {
	tag, err := o.pool.Exec(ctx,
		"DELETE FROM report_outbox WHERE id = $1 AND lease_token = $2", id, token)
	return leaseResult(tag.RowsAffected(), err)
}

// Retry 记一次失败并安排下次尝试。token 认不上返回 ErrLeaseLost 且不改任何列。
func (o *Outbox) Retry(ctx context.Context, id int64, token [16]byte,
	nextAt time.Time, lastErr string) error {

	tag, err := o.pool.Exec(ctx, `
		UPDATE report_outbox
		SET attempts = attempts + 1, next_attempt_at = $3, last_error = $4,
		    lease_until = NULL, lease_token = NULL
		WHERE id = $1 AND lease_token = $2`,
		id, token, nextAt, truncate(lastErr, 512))
	return leaseResult(tag.RowsAffected(), err)
}

// leaseResult 把「零行受影响」翻成 ErrLeaseLost。
//
// 收成一个函数是因为三个写入点都要这么判，而漏判一处的后果是那一处退回盲写：
// 语句里带着 token 条件却不看结果，等于只是让写入静默失效。
func leaseResult(affected int64, err error) error {
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrLeaseLost
	}
	return nil
}

// deadHorizon 是死信项的 next_attempt_at：远到 worker 永远不会取到，
// 但行还在，管理面能看到、能手动重试。不静默丢弃是刻意的。
var deadHorizon = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// Bury 把项标成死信：保留行供管理面查看与手动重试。
// token 认不上返回 ErrLeaseLost 且不把行推到地平线。
func (o *Outbox) Bury(ctx context.Context, id int64, token [16]byte, lastErr string) error {
	tag, err := o.pool.Exec(ctx, `
		UPDATE report_outbox
		SET attempts = attempts + 1, next_attempt_at = $3, last_error = $4,
		    lease_until = NULL, lease_token = NULL
		WHERE id = $1 AND lease_token = $2`,
		id, token, deadHorizon, truncate(lastErr, 512))
	return leaseResult(tag.RowsAffected(), err)
}

// Revive 把死信项重新排到现在，供管理面的重试按钮使用。
//
// 一并清掉租约，这是这个函数里最要紧的一行：worker 手里可能正攥着这一行的
// 旧 token，清掉之后它那笔迟到的裁决会认不上而失效。不清的话运维点了重试，
// 界面上看着回到了待发，下一秒又被旧快照埋回地平线，而日志里没有任何一行
// 说明是谁埋的。
func (o *Outbox) Revive(ctx context.Context, reportID string) error {
	tag, err := o.pool.Exec(ctx, `
		UPDATE report_outbox
		SET next_attempt_at = now(), attempts = 0,
		    lease_until = NULL, lease_token = NULL
		WHERE report_id = $1`, reportID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// State 是列表筛选用的队列状态。
type State string

const (
	StatePending State = "pending"
	StateDead    State = "dead"
	StateAll     State = ""
)

// List 按状态列出队列项，供管理面查看。
func (o *Outbox) List(ctx context.Context, state State, limit int) ([]Entry, error) {
	if limit <= 0 || limit > maxLimit {
		limit = 100
	}
	query := `SELECT id, report_id, report_json, attempts, next_attempt_at, last_error, created_at
		FROM report_outbox`
	args := []any{limit}
	switch state {
	case StatePending:
		query += " WHERE next_attempt_at < $2"
		args = append(args, deadHorizon)
	case StateDead:
		query += " WHERE next_attempt_at >= $2"
		args = append(args, deadHorizon)
	}
	query += " ORDER BY created_at DESC LIMIT $1"

	rows, err := o.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Entry{}
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Counts 统计待上报与死信数量，供健康检查。
func (o *Outbox) Counts(ctx context.Context) (pending, dead int, err error) {
	err = o.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE next_attempt_at < $1),
		       count(*) FILTER (WHERE next_attempt_at >= $1)
		FROM report_outbox`, deadHorizon).Scan(&pending, &dead)
	return pending, dead, err
}

type scanner interface {
	Scan(dest ...any) error
}

func scanEntry(row scanner) (Entry, error) {
	var (
		e   Entry
		raw []byte
	)
	if err := row.Scan(&e.ID, &e.ReportID, &raw, &e.Attempts,
		&e.NextAttemptAt, &e.LastError, &e.CreatedAt); err != nil {
		return e, err
	}
	if err := json.Unmarshal(raw, &e.Report); err != nil {
		return e, fmt.Errorf("decode report %s: %w", e.ReportID, err)
	}
	return e, nil
}

// scanClaimed 扫认领语句多出来的那一列。
//
// 与 scanEntry 分开而不是加个参数：List 那条路不认领，让它扫一个必然为 NULL
// 的列只会逼出一个指针类型和一处解引用。
func scanClaimed(row scanner) (Entry, error) {
	var (
		e   Entry
		raw []byte
	)
	if err := row.Scan(&e.ID, &e.ReportID, &raw, &e.Attempts,
		&e.NextAttemptAt, &e.LastError, &e.CreatedAt, &e.LeaseToken); err != nil {
		return e, err
	}
	if err := json.Unmarshal(raw, &e.Report); err != nil {
		return e, fmt.Errorf("decode report %s: %w", e.ReportID, err)
	}
	return e, nil
}

// truncate 限长并保证结果能写进 TEXT 列。
//
// last_error 的来源是上游/调度层的响应字节，按字节切会切在多字节字符中间，
// 而 PG 对非法序列直接拒收整行——这一列存在的意义就是记下那次失败，
// 因为写不进去而丢掉它是最坏的结果。净化与收边界都要做：前者管上游本来就
// 发的坏字节，后者管我们切出来的。
func truncate(s string, max int) string {
	return textsafe.Truncate(textsafe.Clean(s), max)
}
