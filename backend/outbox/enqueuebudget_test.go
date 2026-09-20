package outbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/relayclient"
	"github.com/aceaura/model-surge-agent/backend/store"
)

// hangingQueue 的 Enqueue 一直阻塞到 ctx 结束，模拟 PG hang 住（不是拒连）。
//
// 拒连会立刻返回错误，那条路径本来就不会挂住任何人；真正危险的是 TCP 连上了
// 但对端不回——这时没有超时的入队会永远等下去。
type hangingQueue struct {
	// entered 在 Enqueue 被调用时关闭，便于断言真的走到了兜底路径。
	entered chan struct{}
}

func (q *hangingQueue) Enqueue(ctx context.Context, _ relayclient.ResultReport, _ string) error {
	select {
	case <-q.entered:
	default:
		close(q.entered)
	}
	<-ctx.Done()
	return ctx.Err()
}

func (q *hangingQueue) Due(context.Context, time.Time, int, time.Duration) ([]store.Entry, error) {
	return nil, nil
}
func (q *hangingQueue) Done(context.Context, int64, [16]byte) error { return nil }
func (q *hangingQueue) Retry(context.Context, int64, [16]byte, time.Time, string) error {
	return nil
}
func (q *hangingQueue) Bury(context.Context, int64, [16]byte, string) error { return nil }

// 直投失败后的兜底入队必须带自己的超时预算。
//
// Report 跑在 per-request 上报队列的单消费者里，而请求收尾要等消费者排空：
// 入队无限等待时，客户端会越过它自己全部的预算（请求总时长、首字、idle）
// 一直挂住，服务端也不报任何错。
func TestEnqueueCarriesItsOwnTimeoutBudget(t *testing.T) {
	q := &hangingQueue{entered: make(chan struct{})}
	w := &Worker{
		Queue:  q,
		Sender: senderFunc(func(context.Context, relayclient.ResultReport) error { return errors.New("upstream down") }),
		Opts:   Options{Timeout: 150 * time.Millisecond},
	}

	done := make(chan struct{})
	go func() {
		w.Report(relayclient.ResultReport{ReportID: "rep-hang"})
		close(done)
	}()

	select {
	case <-q.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("直投失败后没有走到兜底入队")
	}

	// 留足余量但远小于「永远」：断言的是有界，不是某个具体时长。
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("入队阻塞时 Report 没有在预算内返回——请求收尾会被它无限拖住")
	}
}

// 用直投失败的 ctx 去入队是不行的：直投往往正是因为超时才失败的，
// 那个预算已经耗尽，拿它入队必然立刻失败——比没有超时更坏。
func TestEnqueueDoesNotInheritTheExhaustedSendContext(t *testing.T) {
	var gotErr error
	q := &ctxProbeQueue{onEnqueue: func(ctx context.Context) error {
		gotErr = ctx.Err()
		return nil
	}}
	w := &Worker{
		Queue: q,
		Sender: senderFunc(func(ctx context.Context, _ relayclient.ResultReport) error {
			// 把直投的预算耗尽，正如真超时那样。
			<-ctx.Done()
			return ctx.Err()
		}),
		Opts: Options{Timeout: 50 * time.Millisecond},
	}
	w.Report(relayclient.ResultReport{ReportID: "rep-expired"})

	if !q.called {
		t.Fatal("直投超时后没有入队")
	}
	if gotErr != nil {
		t.Fatalf("入队拿到的 context 已经是结束态（%v）——说明复用了直投那个耗尽的预算", gotErr)
	}
}

type senderFunc func(context.Context, relayclient.ResultReport) error

func (f senderFunc) Report(ctx context.Context, rep relayclient.ResultReport) error {
	return f(ctx, rep)
}

type ctxProbeQueue struct {
	called    bool
	onEnqueue func(ctx context.Context) error
}

func (q *ctxProbeQueue) Enqueue(ctx context.Context, _ relayclient.ResultReport, _ string) error {
	q.called = true
	return q.onEnqueue(ctx)
}
func (q *ctxProbeQueue) Due(context.Context, time.Time, int, time.Duration) ([]store.Entry, error) {
	return nil, nil
}
func (q *ctxProbeQueue) Done(context.Context, int64, [16]byte) error { return nil }
func (q *ctxProbeQueue) Retry(context.Context, int64, [16]byte, time.Time, string) error {
	return nil
}
func (q *ctxProbeQueue) Bury(context.Context, int64, [16]byte, string) error { return nil }
