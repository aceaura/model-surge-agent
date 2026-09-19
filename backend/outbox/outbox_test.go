package outbox

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/relayclient"
	"github.com/aceaura/model-surge-agent/backend/store"
)

// fakeQueue 是内存队列，行为对齐 store.Outbox 的语义（含 report_id 幂等）。
type fakeQueue struct {
	mu      sync.Mutex
	entries []store.Entry
	nextID  int64
	// enqueueErr 模拟落库也失败的情况。
	enqueueErr error
	buried     []string
	// 租约状态，对齐 lease_until / lease_token 两列。
	leaseUntil map[int64]time.Time
	nextToken  byte
}

// newFakeQueue 建一个租约状态已初始化的队列。
func newFakeQueue() *fakeQueue {
	return &fakeQueue{leaseUntil: map[int64]time.Time{}}
}

func (q *fakeQueue) Enqueue(_ context.Context, rep relayclient.ResultReport, lastErr string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.enqueueErr != nil {
		return q.enqueueErr
	}
	for _, e := range q.entries {
		if e.ReportID == rep.ReportID {
			return nil
		}
	}
	q.nextID++
	q.entries = append(q.entries, store.Entry{
		ID: q.nextID, ReportID: rep.ReportID, Report: rep,
		NextAttemptAt: time.Now(), LastError: lastErr,
	})
	return nil
}

// Due 认领并发一个新 token，行为对齐 store.Outbox。
func (q *fakeQueue) Due(_ context.Context, now time.Time, limit int, lease time.Duration) ([]store.Entry, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := []store.Entry{}
	for i, e := range q.entries {
		if e.NextAttemptAt.After(now) {
			continue
		}
		if !q.leaseUntil[e.ID].IsZero() && q.leaseUntil[e.ID].After(now) {
			continue
		}
		q.nextToken++
		tok := [16]byte{}
		tok[0] = q.nextToken
		q.entries[i].LeaseToken = tok
		q.leaseUntil[e.ID] = now.Add(lease)
		out = append(out, q.entries[i])
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// held 判 token 是否仍是这一行当前的租约。
func (q *fakeQueue) held(id int64, token [16]byte) (int, bool) {
	for i, e := range q.entries {
		if e.ID == id {
			return i, e.LeaseToken == token
		}
	}
	return -1, false
}

func (q *fakeQueue) Done(_ context.Context, id int64, token [16]byte) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	i, ok := q.held(id, token)
	if i < 0 {
		return store.ErrNotFound
	}
	if !ok {
		return store.ErrLeaseLost
	}
	q.entries = append(q.entries[:i], q.entries[i+1:]...)
	return nil
}

func (q *fakeQueue) Retry(_ context.Context, id int64, token [16]byte, nextAt time.Time, lastErr string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	i, ok := q.held(id, token)
	if i < 0 {
		return store.ErrNotFound
	}
	if !ok {
		return store.ErrLeaseLost
	}
	q.entries[i].Attempts++
	q.entries[i].NextAttemptAt = nextAt
	q.entries[i].LastError = lastErr
	q.entries[i].LeaseToken = [16]byte{}
	delete(q.leaseUntil, id)
	return nil
}

func (q *fakeQueue) Bury(_ context.Context, id int64, token [16]byte, lastErr string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	i, ok := q.held(id, token)
	if i < 0 {
		return store.ErrNotFound
	}
	if !ok {
		return store.ErrLeaseLost
	}
	q.entries[i].Attempts++
	q.entries[i].LastError = lastErr
	// 排到永远取不到的将来，但行保留。
	q.entries[i].NextAttemptAt = time.Now().AddDate(1000, 0, 0)
	q.entries[i].LeaseToken = [16]byte{}
	delete(q.leaseUntil, id)
	q.buried = append(q.buried, q.entries[i].ReportID)
	return nil
}

func (q *fakeQueue) size() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.entries)
}

func (q *fakeQueue) entry(t *testing.T, i int) store.Entry {
	t.Helper()
	q.mu.Lock()
	defer q.mu.Unlock()
	if i >= len(q.entries) {
		t.Fatalf("queue has %d entries, wanted index %d", len(q.entries), i)
	}
	return q.entries[i]
}

// fakeSender 按调用序号决定成功与否，并记录收到的上报。
type fakeSender struct {
	mu     sync.Mutex
	got    []relayclient.ResultReport
	failN  int // 前 failN 次失败
	always bool
	calls  int
}

func (s *fakeSender) Report(_ context.Context, rep relayclient.ResultReport) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.always || s.calls <= s.failN {
		return errors.New("relay down")
	}
	s.got = append(s.got, rep)
	return nil
}

func (s *fakeSender) delivered() []relayclient.ResultReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]relayclient.ResultReport(nil), s.got...)
}

func (s *fakeSender) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func report(id string) relayclient.ResultReport {
	return relayclient.ResultReport{
		ReportID: id, RequestID: "req-1", ModelID: "kimi-1/k3",
		Outcome: relayclient.OutcomeNormal,
		Usage:   relayclient.Usage{InputTokens: 120, OutputTokens: 64},
	}
}

func newWorker(q *fakeQueue, s *fakeSender) *Worker {
	return &Worker{
		Queue: q, Sender: s,
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Opts: Options{MaxAttempts: 3, BatchSize: 10, Timeout: time.Second},
	}
}

// 直报成功就不该落库：出箱只是失败兜底。
func TestDirectReportSkipsTheQueue(t *testing.T) {
	q, s := newFakeQueue(), &fakeSender{}
	newWorker(q, s).Report(report("req-1:0"))

	if got := s.delivered(); len(got) != 1 || got[0].ReportID != "req-1:0" {
		t.Fatalf("delivered = %+v", got)
	}
	if q.size() != 0 {
		t.Fatalf("a successful report must not be queued")
	}
}

func TestFailedReportIsQueuedWithTheError(t *testing.T) {
	q, s := newFakeQueue(), &fakeSender{always: true}
	newWorker(q, s).Report(report("req-1:0"))

	if q.size() != 1 {
		t.Fatalf("queue size = %d, a failed report must be persisted", q.size())
	}
	e := q.entry(t, 0)
	if e.ReportID != "req-1:0" || e.LastError == "" {
		t.Fatalf("entry = %+v, the failure reason must be recorded", e)
	}
	if e.Report.Usage.InputTokens != 120 {
		t.Errorf("usage must survive the round trip: %+v", e.Report.Usage)
	}
}

// 落库也失败时只能记日志，但不能 panic 或阻塞数据面。
func TestReportSurvivesQueueFailure(t *testing.T) {
	q := &fakeQueue{enqueueErr: errors.New("pg down")}
	s := &fakeSender{always: true}
	newWorker(q, s).Report(report("req-1:0"))

	if q.size() != 0 {
		t.Fatalf("nothing should have been stored")
	}
}

// 入队后 worker 重放：先失败再成功，最终删行。
func TestDrainReplaysUntilDelivered(t *testing.T) {
	q := newFakeQueue()
	s := &fakeSender{failN: 2} // 直报 + 第一轮重放都失败
	w := newWorker(q, s)

	w.Report(report("req-1:0"))
	if q.size() != 1 {
		t.Fatalf("queue size = %d", q.size())
	}

	// 第一轮：仍失败，退避重排。
	if sent := w.Drain(context.Background()); sent != 0 {
		t.Fatalf("sent = %d, want 0", sent)
	}
	if q.size() != 1 {
		t.Fatalf("entry must stay queued after a failed replay")
	}
	e := q.entry(t, 0)
	if e.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", e.Attempts)
	}
	if !e.NextAttemptAt.After(time.Now()) {
		t.Error("a failed replay must be pushed into the future")
	}

	// 时间推到退避之后，第二轮成功。
	w.Now = func() time.Time { return time.Now().Add(time.Hour) }
	if sent := w.Drain(context.Background()); sent != 1 {
		t.Fatalf("sent = %d, want 1", sent)
	}
	if q.size() != 0 {
		t.Fatalf("a delivered entry must be removed")
	}
	if got := s.delivered(); len(got) != 1 || got[0].ReportID != "req-1:0" {
		t.Fatalf("delivered = %+v", got)
	}
}

// 退避递增且有上限：上限是为了让调度层恢复后队列能及时排空。
func TestBackoffGrowsAndCaps(t *testing.T) {
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{8, 256 * time.Second},
		{20, 5 * time.Minute},
		{1000, 5 * time.Minute},
	}
	for _, c := range cases {
		if got := backoff(c.attempts); got != c.want {
			t.Errorf("backoff(%d) = %v, want %v", c.attempts, got, c.want)
		}
	}
}

// 试满就转死信：保留行供管理面查看，不静默丢弃。
func TestExhaustedEntryIsBuriedNotDeleted(t *testing.T) {
	q := newFakeQueue()
	s := &fakeSender{always: true}
	w := newWorker(q, s)
	w.Opts.MaxAttempts = 2

	w.Report(report("req-1:0"))

	// 第一轮：attempts 0→1，仍在轮转内。
	w.Drain(context.Background())
	if e := q.entry(t, 0); e.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", e.Attempts)
	}

	// 第二轮：attempts 到上限，转死信。
	w.Now = func() time.Time { return time.Now().Add(time.Hour) }
	w.Drain(context.Background())

	if q.size() != 1 {
		t.Fatalf("a buried entry must stay in the table, got %d rows", q.size())
	}
	if len(q.buried) != 1 || q.buried[0] != "req-1:0" {
		t.Fatalf("buried = %v", q.buried)
	}

	// 再也不会被取到。
	w.Now = func() time.Time { return time.Now().AddDate(100, 0, 0) }
	if sent := w.Drain(context.Background()); sent != 0 {
		t.Fatalf("a buried entry must not come back into rotation")
	}
}

// report_id 幂等：同一份重复入队不会变成两条，重放也不会重复计数。
func TestSameReportIDIsNotQueuedTwice(t *testing.T) {
	q := newFakeQueue()
	s := &fakeSender{always: true}
	w := newWorker(q, s)

	for range 3 {
		w.Report(report("req-1:0"))
	}
	if q.size() != 1 {
		t.Fatalf("queue size = %d, the same report_id must collapse", q.size())
	}
}

func TestDrainHandlesBatchInOnePass(t *testing.T) {
	q := newFakeQueue()
	s := &fakeSender{always: true}
	w := newWorker(q, s)

	for i := range 5 {
		w.Report(report("req-1:" + string(rune('0'+i))))
	}
	if q.size() != 5 {
		t.Fatalf("queue size = %d", q.size())
	}

	s.always = false
	before := s.callCount()
	if sent := w.Drain(context.Background()); sent != 5 {
		t.Fatalf("sent = %d, want the whole batch", sent)
	}
	if s.callCount()-before != 5 {
		t.Errorf("sender calls = %d, want 5", s.callCount()-before)
	}
	if q.size() != 0 {
		t.Errorf("%d entries left", q.size())
	}
}

// ctx 取消时立刻停手，不要把剩下的批次跑完。
func TestDrainStopsOnCancelledContext(t *testing.T) {
	q := newFakeQueue()
	s := &fakeSender{always: true}
	w := newWorker(q, s)
	for i := range 3 {
		w.Report(report("req-1:" + string(rune('0'+i))))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sent := w.Drain(ctx); sent != 0 {
		t.Fatalf("sent = %d on a cancelled context", sent)
	}
}

func TestRunDrainsUntilContextEnds(t *testing.T) {
	q := newFakeQueue()
	s := &fakeSender{always: true}
	w := newWorker(q, s)
	w.Opts.Interval = 5 * time.Millisecond
	w.Report(report("req-1:0"))

	s.always = false
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	deadline := time.After(time.Second)
	for q.size() > 0 {
		select {
		case <-deadline:
			t.Fatal("Run never drained the queue")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}
