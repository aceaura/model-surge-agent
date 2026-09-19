package outbox

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/store"
)

// leaseLostQueue 让指定的写入点回 ErrLeaseLost，其余照常。
type leaseLostQueue struct {
	*fakeQueue
	on string
}

func (q *leaseLostQueue) Retry(ctx context.Context, id int64, token [16]byte,
	nextAt time.Time, lastErr string) error {
	if q.on == "retry" {
		return store.ErrLeaseLost
	}
	return q.fakeQueue.Retry(ctx, id, token, nextAt, lastErr)
}

func (q *leaseLostQueue) Done(ctx context.Context, id int64, token [16]byte) error {
	if q.on == "done" {
		return store.ErrLeaseLost
	}
	return q.fakeQueue.Done(ctx, id, token)
}

// 租约丢失不得中断这一批的其余条目。
//
// 单进程里丢租约是常态而非故障：关停期那次 Drain 与 ticker 那次并存，
// 一方拿着已过期的租约做裁决就会走到这里。若把它当成错误而提前收手，
// 一次偶发的重叠会让整批到期上报停在原地。
func TestLeaseLostOnRetryDoesNotStopTheBatch(t *testing.T) {
	q := &leaseLostQueue{fakeQueue: newFakeQueue(), on: "retry"}
	for _, id := range []string{"a", "b", "c"} {
		if err := q.Enqueue(context.Background(), report(id), "boom"); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	// 三条全失败，于是三条都会走 Retry。
	s := &fakeSender{always: true}
	w := &Worker{Queue: q, Sender: s, Log: quietLogger(), Opts: Options{MaxAttempts: 20}}

	w.Drain(context.Background())

	if got := s.callCount(); got != 3 {
		t.Errorf("只尝试了 %d 条，want 3——丢租约把整批中断了", got)
	}
}

// 报已经送达了，所以删行时丢租约仍要计入 sent。
//
// 不计的话调用方（关停期的那次 Drain）会以为什么都没冲出去，
// 而日志里那句「flushed N pending reports」就永远是 0。
func TestLeaseLostOnDoneStillCountsAsSent(t *testing.T) {
	q := &leaseLostQueue{fakeQueue: newFakeQueue(), on: "done"}
	if err := q.Enqueue(context.Background(), report("a"), "boom"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	w := &Worker{Queue: q, Sender: &fakeSender{}, Log: quietLogger()}

	if got := w.Drain(context.Background()); got != 1 {
		t.Errorf("sent = %d, want 1——报确实送达了", got)
	}
}

// 认领要带租约长度，且长度随单条上报超时放大。
//
// 租约短于上报超时的话，一条仍在飞的上报会被另一个执行流重新认领，
// 于是同一个报发两次、attempts 又开始虚涨。
func TestClaimLeaseOutlivesTheSendTimeout(t *testing.T) {
	q := &recordingQueue{fakeQueue: newFakeQueue()}
	w := &Worker{Queue: q, Sender: &fakeSender{}, Log: quietLogger(),
		Opts: Options{Timeout: 40 * time.Second}}

	w.Drain(context.Background())

	if q.lease <= w.timeout() {
		t.Errorf("租约 %s 不长于上报超时 %s，在飞的上报会被抢走",
			q.lease, w.timeout())
	}
}

// 默认配置下租约也必须有下限，不能是零。
func TestClaimLeaseHasFloor(t *testing.T) {
	q := &recordingQueue{fakeQueue: newFakeQueue()}
	w := &Worker{Queue: q, Sender: &fakeSender{}, Log: quietLogger()}

	w.Drain(context.Background())

	if q.lease < 30*time.Second {
		t.Errorf("默认租约 %s 短于 30s 下限", q.lease)
	}
}

// recordingQueue 记下认领时传进来的租约长度。
type recordingQueue struct {
	*fakeQueue
	lease time.Duration
}

func (q *recordingQueue) Due(ctx context.Context, now time.Time, limit int,
	lease time.Duration) ([]store.Entry, error) {
	q.lease = lease
	return q.fakeQueue.Due(ctx, now, limit, lease)
}

// quietLogger 丢掉日志：这些用例刻意触发 Debug/Error 分支。
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
