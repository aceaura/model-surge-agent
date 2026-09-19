package store

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"
)

// 两个执行流不得拿到同一行。
//
// 此前 Due 只是 SELECT，两次相邻调用返回完全相同的三行（真 PG 上实测过）。
// 后果是两边各把同一次失败记一笔：attempts 以双倍速度烧完 MaxAttempts，
// 一条只是撞上调度层短暂抖动的上报被提前判死，而管理面显示的是
// 「重试了 20 次仍失败」——运维据此得出的结论是假的。
func TestConcurrentClaimsAreDisjoint(t *testing.T) {
	o, ctx := freshOutbox(t)
	seedOutbox(t, o, 3)

	now := time.Now()
	first, err := o.Due(ctx, now, 10, time.Minute)
	if err != nil {
		t.Fatalf("first Due: %v", err)
	}
	second, err := o.Due(ctx, now, 10, time.Minute)
	if err != nil {
		t.Fatalf("second Due: %v", err)
	}

	if len(first) != 3 {
		t.Fatalf("第一次认领到 %d 条，want 3", len(first))
	}
	seen := map[int64]bool{}
	for _, e := range first {
		seen[e.ID] = true
	}
	for _, e := range second {
		if seen[e.ID] {
			t.Errorf("id %d 被两个执行流同时认领，attempts 会记成两次", e.ID)
		}
	}
}

// 全部到期项被取走后，第二个执行流应当拿到空批而不是重复批。
func TestSecondClaimGetsNothingWhenAllLeased(t *testing.T) {
	o, ctx := freshOutbox(t)
	seedOutbox(t, o, 2)

	now := time.Now()
	if _, err := o.Due(ctx, now, 10, time.Minute); err != nil {
		t.Fatalf("Due: %v", err)
	}
	left, err := o.Due(ctx, now, 10, time.Minute)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("租约仍有效时又认领到 %d 条", len(left))
	}
}

// 租约到期后行必须重新可见：持有者可能已经随进程被杀掉了。
func TestExpiredLeaseBecomesClaimableAgain(t *testing.T) {
	o, ctx := freshOutbox(t)
	seedOutbox(t, o, 1)

	now := time.Now()
	if _, err := o.Due(ctx, now, 10, 5*time.Second); err != nil {
		t.Fatalf("Due: %v", err)
	}
	// 租约过期之后的一个时刻。
	again, err := o.Due(ctx, now.Add(6*time.Second), 10, time.Minute)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(again) != 1 {
		t.Errorf("租约过期后认领到 %d 条，want 1——否则这一行永久隐身", len(again))
	}
}

// 持租约的一次失败恰好记一笔。
func TestRetryWithLeaseCountsExactlyOnce(t *testing.T) {
	o, ctx := freshOutbox(t)
	seedOutbox(t, o, 1)

	due, err := o.Due(ctx, time.Now(), 10, time.Minute)
	if err != nil || len(due) != 1 {
		t.Fatalf("Due: %d 条, err = %v", len(due), err)
	}
	next := time.Now().Add(time.Minute).UTC().Truncate(time.Millisecond)
	if err := o.Retry(ctx, due[0].ID, due[0].LeaseToken, next, "relay down"); err != nil {
		t.Fatalf("Retry: %v", err)
	}

	row := readRow(t, ctx, o, due[0].ID)
	if row.attempts != 1 {
		t.Errorf("attempts = %d, want 1", row.attempts)
	}
	if !row.nextAt.Equal(next) {
		t.Errorf("next_attempt_at = %s, want %s", row.nextAt, next)
	}
	if row.lastError != "relay down" {
		t.Errorf("last_error = %q", row.lastError)
	}
}

// 丢了租约的 Retry 不得改任何一列。
//
// 三列全读回来比对：只判返回值的话，一个「带着 token 条件但不看 RowsAffected」
// 的实现会照样报成功，而只判 attempts 的话，一个改了 next_attempt_at 却没动
// 计数的实现也能过。
func TestRetryWithLostLeaseChangesNothing(t *testing.T) {
	o, ctx := freshOutbox(t)
	seedOutbox(t, o, 1)

	due, err := o.Due(ctx, time.Now(), 10, time.Minute)
	if err != nil || len(due) != 1 {
		t.Fatalf("Due: %d 条, err = %v", len(due), err)
	}
	before := readRow(t, ctx, o, due[0].ID)

	stale := [16]byte{0xde, 0xad}
	err = o.Retry(ctx, due[0].ID, stale, time.Now().Add(time.Hour), "stale verdict")
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("Retry 用作废的 token 返回 %v, want ErrLeaseLost", err)
	}

	after := readRow(t, ctx, o, due[0].ID)
	if after.attempts != before.attempts {
		t.Errorf("attempts 被改成 %d，原本 %d", after.attempts, before.attempts)
	}
	if !after.nextAt.Equal(before.nextAt) {
		t.Errorf("next_attempt_at 被改成 %s，原本 %s", after.nextAt, before.nextAt)
	}
	if after.lastError != before.lastError {
		t.Errorf("last_error 被改成 %q，原本 %q", after.lastError, before.lastError)
	}
}

// 丢了租约的 Bury 不得把行推到地平线。
//
// 这是最坏的那个形态：一条本可以继续重试的上报被一个已经失效的裁决判死，
// 而运维在管理面看到它躺在死信里。
func TestBuryWithLostLeaseDoesNotDeadLetter(t *testing.T) {
	o, ctx := freshOutbox(t)
	seedOutbox(t, o, 1)

	due, err := o.Due(ctx, time.Now(), 10, time.Minute)
	if err != nil || len(due) != 1 {
		t.Fatalf("Due: %d 条, err = %v", len(due), err)
	}

	stale := [16]byte{0xbe, 0xef}
	if err := o.Bury(ctx, due[0].ID, stale, "stale verdict"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("Bury 用作废的 token 返回 %v, want ErrLeaseLost", err)
	}
	if got := readRow(t, ctx, o, due[0].ID).nextAt; got.Equal(deadHorizon) {
		t.Error("失效的裁决把行埋进了死信")
	}
}

// 丢了租约的 Done 不得删行。
func TestDoneWithLostLeaseKeepsRow(t *testing.T) {
	o, ctx := freshOutbox(t)
	seedOutbox(t, o, 1)

	due, err := o.Due(ctx, time.Now(), 10, time.Minute)
	if err != nil || len(due) != 1 {
		t.Fatalf("Due: %d 条, err = %v", len(due), err)
	}

	stale := [16]byte{0x01}
	if err := o.Done(ctx, due[0].ID, stale); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("Done 用作废的 token 返回 %v, want ErrLeaseLost", err)
	}
	if _, _, err := o.Counts(ctx); err != nil {
		t.Fatalf("Counts: %v", err)
	}
	if !rowExists(t, ctx, o, due[0].ID) {
		t.Error("失效的 Done 把行删了")
	}
}

// Revive 之后 worker 手里的旧快照不得把行重新埋掉。
//
// 这是真 PG 上探针跑出来的形态：Bury → Revive → 旧快照再 Bury，
// next_attempt_at 回到 9999。运维点了重试，界面上看着回到待发，下一秒
// 又变成死信，而日志里没有任何一行说明是谁埋的。
func TestReviveInvalidatesWorkerSnapshot(t *testing.T) {
	o, ctx := freshOutbox(t)
	seedOutbox(t, o, 1)

	due, err := o.Due(ctx, time.Now(), 10, time.Minute)
	if err != nil || len(due) != 1 {
		t.Fatalf("Due: %d 条, err = %v", len(due), err)
	}
	snapshot := due[0]

	if err := o.Revive(ctx, snapshot.ReportID); err != nil {
		t.Fatalf("Revive: %v", err)
	}
	// worker 攥着 Revive 之前的 token，此刻做出裁决。
	if err := o.Bury(ctx, snapshot.ID, snapshot.LeaseToken, "too late"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("Revive 之后旧 token 的 Bury 返回 %v, want ErrLeaseLost", err)
	}
	if got := readRow(t, ctx, o, snapshot.ID).nextAt; got.Equal(deadHorizon) {
		t.Error("Revive 被一个迟到的裁决撤销了")
	}
}

// Revive 归零计数并清空租约。
func TestReviveClearsAttemptsAndLease(t *testing.T) {
	o, ctx := freshOutbox(t)
	seedOutbox(t, o, 1)

	due, _ := o.Due(ctx, time.Now(), 10, time.Minute)
	if err := o.Retry(ctx, due[0].ID, due[0].LeaseToken, time.Now(), "boom"); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if err := o.Revive(ctx, due[0].ReportID); err != nil {
		t.Fatalf("Revive: %v", err)
	}

	row := readRow(t, ctx, o, due[0].ID)
	if row.attempts != 0 {
		t.Errorf("attempts = %d, want 0", row.attempts)
	}
	if row.leased {
		t.Error("Revive 之后仍有租约在，worker 的旧裁决还能生效")
	}
}

// limit 为 0 仍取 100 条：现有语义不变。
func TestClaimLimitZeroKeepsHundred(t *testing.T) {
	o, ctx := freshOutbox(t)
	seedOutbox(t, o, 101)

	due, err := o.Due(ctx, time.Now(), 0, time.Minute)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(due) != 100 {
		t.Errorf("limit=0 取到 %d 条, want 100", len(due))
	}
}

// 老库升级路径：两列必须由 ALTER 补上，而不是只出现在 CREATE TABLE 里。
//
// CREATE TABLE IF NOT EXISTS 对已存在的表整段跳过，所以先于这两列建起的
// 库升级上来之后会拿不到它们，而认领语句会整条报错——outbox 彻底停转。
func TestLeaseColumnsAreAddedToOldTables(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	for _, col := range []string{"lease_until", "lease_token"} {
		if _, err := s.pool.Exec(ctx,
			"ALTER TABLE report_outbox DROP COLUMN IF EXISTS "+col); err != nil {
			t.Fatalf("drop %s: %v", col, err)
		}
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, col := range []string{"lease_until", "lease_token"} {
		var exists bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.columns
			 WHERE table_name = 'report_outbox' AND column_name = $1)`,
			col).Scan(&exists); err != nil {
			t.Fatalf("check %s: %v", col, err)
		}
		if !exists {
			t.Errorf("升级后仍没有 %s 列，认领语句会整条失败", col)
		}
	}
	// 补回来之后认领要真的能用：只判列存在的话，一个类型写错的 ALTER
	// 也能过。
	o := NewOutbox(s.pool)
	if err := o.Enqueue(ctx, report("rep-after-lease-migrate"), ""); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := o.Due(ctx, time.Now(), 10, time.Minute); err != nil {
		t.Fatalf("升级后认领失败: %v", err)
	}
}

// 被认领但仍在飞的行仍算 pending：它确实还没送达。
func TestCountsTreatsLeasedRowsAsPending(t *testing.T) {
	o, ctx := freshOutbox(t)
	seedOutbox(t, o, 2)

	if _, err := o.Due(ctx, time.Now(), 10, time.Minute); err != nil {
		t.Fatalf("Due: %v", err)
	}
	pending, dead, err := o.Counts(ctx)
	if err != nil {
		t.Fatalf("Counts: %v", err)
	}
	if pending != 2 || dead != 0 {
		t.Errorf("pending/dead = %d/%d, want 2/0——在飞的行不该从积压数里消失",
			pending, dead)
	}
}

// freshOutbox 给一张空表，避免并行/前序用例留下的行干扰认领计数。
func freshOutbox(t *testing.T) (*Outbox, context.Context) {
	t.Helper()
	ctx := context.Background()
	s := open(t)
	if _, err := s.pool.Exec(ctx, "DELETE FROM report_outbox"); err != nil {
		t.Fatalf("clear outbox: %v", err)
	}
	return NewOutbox(s.pool), ctx
}

func seedOutbox(t *testing.T, o *Outbox, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := o.Enqueue(context.Background(),
			report("lease-"+strconv.Itoa(i)), "boom"); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
}

type outboxRow struct {
	attempts  int
	nextAt    time.Time
	lastError string
	leased    bool
}

func readRow(t *testing.T, ctx context.Context, o *Outbox, id int64) outboxRow {
	t.Helper()
	var (
		r     outboxRow
		token *[16]byte
	)
	if err := o.pool.QueryRow(ctx,
		`SELECT attempts, next_attempt_at, last_error, lease_token
		 FROM report_outbox WHERE id = $1`, id).
		Scan(&r.attempts, &r.nextAt, &r.lastError, &token); err != nil {
		t.Fatalf("read row %d: %v", id, err)
	}
	r.nextAt = r.nextAt.UTC()
	r.leased = token != nil
	return r
}

func rowExists(t *testing.T, ctx context.Context, o *Outbox, id int64) bool {
	t.Helper()
	var exists bool
	if err := o.pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM report_outbox WHERE id = $1)", id).
		Scan(&exists); err != nil {
		t.Fatalf("exists %d: %v", id, err)
	}
	return exists
}
