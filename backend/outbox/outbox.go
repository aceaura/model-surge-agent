// Package outbox 保证结果上报最终送达。
//
// 调度层的冷却判定与用量统计都依赖这些上报，丢一条就会让决策失真。
// 因此直报失败不算完事：落库排队，由后台 worker 指数退避重放。
package outbox

import (
	"context"
	"log/slog"
	"time"

	"github.com/aceaura/model-surge-agent/backend/relayclient"
	"github.com/aceaura/model-surge-agent/backend/store"
)

type Queue interface {
	Enqueue(ctx context.Context, rep relayclient.ResultReport, lastErr string) error
	Due(ctx context.Context, now time.Time, limit int) ([]store.Entry, error)
	Done(ctx context.Context, id int64) error
	Retry(ctx context.Context, id int64, nextAt time.Time, lastErr string) error
	Bury(ctx context.Context, id int64, lastErr string) error
}

type Sender interface {
	Report(ctx context.Context, rep relayclient.ResultReport) error
}

type Options struct {
	// Interval 是扫描到期项的周期。
	Interval time.Duration
	// MaxAttempts 之后转死信：保留行供管理面查看，不静默丢弃。
	MaxAttempts int
	// BatchSize 是单轮处理条数。
	BatchSize int
	// Timeout 是单条上报的超时。
	Timeout time.Duration
}

type Worker struct {
	Queue  Queue
	Sender Sender
	Log    *slog.Logger
	Opts   Options
	// Now 可注入以便测试驱动退避时间。
	Now func() time.Time
}

// Report 实现 pipeline.Reporter：先直报，失败才入队。
//
// 用独立的 context.Background：客户端请求结束时它的 context 就取消了，
// 但上报与那个请求的生命周期无关，必须继续。
func (w *Worker) Report(rep relayclient.ResultReport) {
	ctx, cancel := context.WithTimeout(context.Background(), w.timeout())
	defer cancel()

	err := w.Sender.Report(ctx, rep)
	if err == nil {
		return
	}
	// 不可重试的失败（如契约不符）入队也没用，但仍要落库：
	// 悄悄丢弃会让运维查不出用量为何缺口。管理面能看到 last_error。
	if qerr := w.Queue.Enqueue(context.Background(), rep, err.Error()); qerr != nil {
		w.log().Error("report lost: direct send and enqueue both failed",
			"report_id", rep.ReportID, "send_error", err, "enqueue_error", qerr)
		return
	}
	w.log().Warn("report queued for retry", "report_id", rep.ReportID, "error", err)
}

// Run 循环处理到期项直到 ctx 取消。
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.Drain(ctx)
		}
	}
}

// Drain 处理一批到期项。返回成功送达的条数，供测试与指标使用。
func (w *Worker) Drain(ctx context.Context) int {
	entries, err := w.Queue.Due(ctx, w.now(), w.batchSize())
	if err != nil {
		w.log().Error("outbox scan failed", "error", err)
		return 0
	}

	sent := 0
	for _, e := range entries {
		if ctx.Err() != nil {
			return sent
		}
		sendCtx, cancel := context.WithTimeout(ctx, w.timeout())
		err := w.Sender.Report(sendCtx, e.Report)
		cancel()

		if err == nil {
			if err := w.Queue.Done(ctx, e.ID); err != nil {
				w.log().Error("outbox delete failed", "report_id", e.ReportID, "error", err)
				continue
			}
			sent++
			continue
		}

		attempts := e.Attempts + 1
		if attempts >= w.maxAttempts() {
			// 转死信而非删除：运维需要看到哪些用量没能上报。
			if err := w.Queue.Bury(ctx, e.ID, err.Error()); err != nil {
				w.log().Error("outbox bury failed", "report_id", e.ReportID, "error", err)
			}
			w.log().Error("report given up after max attempts",
				"report_id", e.ReportID, "attempts", attempts, "error", err)
			continue
		}
		if err := w.Queue.Retry(ctx, e.ID, w.now().Add(backoff(attempts)), err.Error()); err != nil {
			w.log().Error("outbox reschedule failed", "report_id", e.ReportID, "error", err)
		}
	}
	return sent
}

// backoff 指数退避，1s 起翻倍，5min 封顶。
// 封顶是为了让调度层恢复后队列能在可接受时间内排空。
func backoff(attempts int) time.Duration {
	const (
		base    = time.Second
		ceiling = 5 * time.Minute
	)
	d := base << min(attempts, 16)
	if d > ceiling {
		return ceiling
	}
	return d
}

func (w *Worker) interval() time.Duration {
	if w.Opts.Interval > 0 {
		return w.Opts.Interval
	}
	return time.Second
}

func (w *Worker) maxAttempts() int {
	if w.Opts.MaxAttempts > 0 {
		return w.Opts.MaxAttempts
	}
	return 20
}

func (w *Worker) batchSize() int {
	if w.Opts.BatchSize > 0 {
		return w.Opts.BatchSize
	}
	return 100
}

func (w *Worker) timeout() time.Duration {
	if w.Opts.Timeout > 0 {
		return w.Opts.Timeout
	}
	return 10 * time.Second
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Worker) log() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}
