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

// Due 取到期的待上报项。limit 为 0 时取 100 条。
func (o *Outbox) Due(ctx context.Context, now time.Time, limit int) ([]Entry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := o.pool.Query(ctx, `
		SELECT id, report_id, report_json, attempts, next_attempt_at, last_error, created_at
		FROM report_outbox
		WHERE next_attempt_at <= $1
		ORDER BY next_attempt_at
		LIMIT $2`, now, limit)
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

// Done 上报成功，删行。
func (o *Outbox) Done(ctx context.Context, id int64) error {
	_, err := o.pool.Exec(ctx, "DELETE FROM report_outbox WHERE id = $1", id)
	return err
}

// Retry 记一次失败并安排下次尝试。
func (o *Outbox) Retry(ctx context.Context, id int64, nextAt time.Time, lastErr string) error {
	_, err := o.pool.Exec(ctx, `
		UPDATE report_outbox
		SET attempts = attempts + 1, next_attempt_at = $2, last_error = $3
		WHERE id = $1`, id, nextAt, truncate(lastErr, 512))
	return err
}

// deadHorizon 是死信项的 next_attempt_at：远到 worker 永远不会取到，
// 但行还在，管理面能看到、能手动重试。不静默丢弃是刻意的。
var deadHorizon = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// Bury 把项标成死信：保留行供管理面查看与手动重试。
func (o *Outbox) Bury(ctx context.Context, id int64, lastErr string) error {
	_, err := o.pool.Exec(ctx, `
		UPDATE report_outbox
		SET attempts = attempts + 1, next_attempt_at = $2, last_error = $3
		WHERE id = $1`, id, deadHorizon, truncate(lastErr, 512))
	return err
}

// Revive 把死信项重新排到现在，供管理面的重试按钮使用。
func (o *Outbox) Revive(ctx context.Context, reportID string) error {
	tag, err := o.pool.Exec(ctx, `
		UPDATE report_outbox SET next_attempt_at = now(), attempts = 0
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

// truncate 限长并保证结果能写进 TEXT 列。
//
// last_error 的来源是上游/调度层的响应字节，按字节切会切在多字节字符中间，
// 而 PG 对非法序列直接拒收整行——这一列存在的意义就是记下那次失败，
// 因为写不进去而丢掉它是最坏的结果。净化与收边界都要做：前者管上游本来就
// 发的坏字节，后者管我们切出来的。
func truncate(s string, max int) string {
	return textsafe.Truncate(textsafe.Clean(s), max)
}
