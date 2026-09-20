// Package outbox 保证结果上报最终送达。
//
// 调度层的冷却判定与用量统计都依赖这些上报，丢一条就会让决策失真。
// 因此直报失败不算完事：落库排队，由后台 worker 指数退避重放。
package outbox

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/aceaura/model-surge-agent/backend/relayclient"
	"github.com/aceaura/model-surge-agent/backend/store"
)

type Queue interface {
	Enqueue(ctx context.Context, rep relayclient.ResultReport, lastErr string) error
	// Due 认领到期项并连同所有权凭据一起返回，lease 是租约长度。
	Due(ctx context.Context, now time.Time, limit int, lease time.Duration) ([]store.Entry, error)
	Done(ctx context.Context, id int64, token [16]byte) error
	Retry(ctx context.Context, id int64, token [16]byte, nextAt time.Time, lastErr string) error
	Bury(ctx context.Context, id int64, token [16]byte, lastErr string) error
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
	//
	// 另起一个 context 而不是复用上面那个：直投往往正是因为超时才失败的，
	// 那个预算已经耗尽，拿它入队必然立刻失败——比没有超时更坏。而完全不设
	// 超时也不行：本函数跑在 per-request 上报队列的单消费者里，而请求收尾
	// 要等消费者排空，PG hang 住（不是拒连）时客户端会越过它自己全部的
	// 预算（请求总时长、首字、idle）一直挂住，且服务端不报任何错。
	qctx, qcancel := context.WithTimeout(context.Background(), w.timeout())
	defer qcancel()
	if qerr := w.Queue.Enqueue(qctx, rep, err.Error()); qerr != nil {
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
	entries, err := w.Queue.Due(ctx, w.now(), w.batchSize(), w.lease())
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
			// 报已经送达了，所以无论删行成不成都算送达。租约丢失只意味着
			// 这一行的所有权换了手，那边会去删。
			if err := w.Queue.Done(ctx, e.ID, e.LeaseToken); err != nil {
				w.logLease("outbox delete", e.ReportID, err)
			}
			sent++
			continue
		}

		attempts := e.Attempts + 1
		if attempts >= w.maxAttempts() {
			// 转死信而非删除：运维需要看到哪些用量没能上报。
			if err := w.Queue.Bury(ctx, e.ID, e.LeaseToken, err.Error()); err != nil {
				w.logLease("outbox bury", e.ReportID, err)
			}
			w.log().Error("report given up after max attempts",
				"report_id", e.ReportID, "attempts", attempts, "error", err)
			continue
		}
		if err := w.Queue.Retry(ctx, e.ID, e.LeaseToken,
			w.now().Add(backoff(attempts)), err.Error()); err != nil {
			w.logLease("outbox reschedule", e.ReportID, err)
		}
	}
	return sent
}

// logLease 把租约丢失与真故障分级。
//
// 丢租约不是故障：租约过期后这一行被另一个执行流接手了，或者管理面点了
// 重试按钮把它收回去了。按 Error 记会让日志周期性地报一个不存在的故障，
// 而运维会去查它。
func (w *Worker) logLease(what, reportID string, err error) {
	if errors.Is(err, store.ErrLeaseLost) {
		w.log().Debug(what+" skipped: lease lost", "report_id", reportID)
		return
	}
	w.log().Error(what+" failed", "report_id", reportID, "error", err)
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

// lease 是认领的租约长度：单条上报超时的三倍，下限 30s。
//
// 从上报超时派生而不是写死：过短会让一条仍在飞的上报被另一个执行流重新认领，
// 于是同一个报发两次（幂等键顶得住，但 attempts 又开始虚涨）；过长会让进程
// 被杀之后的那一批行长时间隐身。
//
// 不按批量条数放大：一批一百条会算出千秒级的租约，而那一百条里绝大多数还没
// 开始处理，让它们背一个千秒的隐身期没有道理。
func (w *Worker) lease() time.Duration {
	if d := 3 * w.timeout(); d > 30*time.Second {
		return d
	}
	return 30 * time.Second
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
