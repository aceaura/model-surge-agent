package main

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// gracefulShutdowner 是 *http.Server 在关停里用到的那一面。
// 收窄成接口是为了能在测试里造出「Shutdown 超时」这一形态——
// 用真服务器造它要挂一条真的在途流，那样测的是 net/http 不是本层顺序。
type gracefulShutdowner interface {
	Shutdown(ctx context.Context) error
}

// pendingFlusher 是 outbox.Worker 在关停里用到的那一面。
type pendingFlusher interface {
	Drain(ctx context.Context) int
}

// inflightWaiter 报告仍在途的请求数，并在降到零或 ctx 到期时返回。
type inflightWaiter interface {
	wait(ctx context.Context) int64
}

// shutdownPlan 是三阶段各自的预算。
type shutdownPlan struct {
	// Grace 是等在途流自然收尾的预算。
	Grace time.Duration
	// Linger 是 Grace 用尽后继续等在途请求的上限。
	Linger time.Duration
	// Flush 是冲 outbox 的预算。
	Flush time.Duration
}

// shutdown 按三阶段收尾。
//
// 每阶段新建 context 而不是从上一阶段派生：派生会把上一阶段刚用尽的
// deadline 继承下来，于是后面那些阶段一进去就已过期。这正是本轮之前的
// 形态——`Shutdown` 烧完 20 秒预算，`Drain` 拿到一个死 context，
// `Queue.Due` 立刻失败，那次「退出前冲一次队列」在它唯一有意义的场景里
// 保证是空操作。
//
// 也不从信号 context 派生：它在 SIGTERM 那一刻就已经 Done。
func shutdown(log *slog.Logger, srv gracefulShutdowner,
	inflight inflightWaiter, flusher pendingFlusher, plan shutdownPlan) {

	graceCtx, cancelGrace := context.WithTimeout(context.Background(), plan.Grace)
	defer cancelGrace()
	err := srv.Shutdown(graceCtx)
	if err != nil {
		log.Warn("graceful shutdown incomplete", "error", err)

		// 只在超时那一支才等：Shutdown 干净返回意味着在途已清零，
		// 再等就是白白拖长部署窗口。
		lingerCtx, cancelLinger := context.WithTimeout(context.Background(), plan.Linger)
		left := inflight.wait(lingerCtx)
		cancelLinger()
		if left > 0 {
			// 这一行是运维唯一能看到「这次部署丢了多少上报」的地方：
			// 被掐断的 handler 连同它那条投递 goroutine 一起随进程死掉，
			// 那些上报既没 POST 出去也没落库。
			log.Warn("gave up waiting for in-flight requests", "count", left)
		}
	}

	// 无条件冲一次，且用一份全新的预算：前两阶段的成败与队列里有没有
	// 到期项无关。
	flushCtx, cancelFlush := context.WithTimeout(context.Background(), plan.Flush)
	defer cancelFlush()
	if sent := flusher.Drain(flushCtx); sent > 0 {
		log.Info("flushed pending reports on shutdown", "count", sent)
	}
}

// newListener 组装监听器。
//
// 存在的理由是把「handler 必须先过在途计数」这件事从 run 里挪到一个
// 测试到得了的地方：留在 run 里的话，漏掉 wrap 那一行不会让任何测试变红，
// 而后果是关停期的等待永远看到零在途、白等一场。
func newListener(addr string, h http.Handler, tracker *inflight) *http.Server {
	return &http.Server{
		Addr:    addr,
		Handler: tracker.wrap(h),
		// 不设 WriteTimeout：SSE 响应会持续数分钟，写超时会把流从中间掐断。
		// 空闲与首字超时由 pipeline 按流的语义控制。
		ReadHeaderTimeout: 15 * time.Second,
	}
}

// inflightPollInterval 是等在途归零时的轮询周期。
//
// 轮询而不是 sync.WaitGroup：WaitGroup.Wait 不接受超时，而关停期的等待
// 必须有界，否则一条卡住的上游会把部署窗口拖到无限。关停是一次性动作，
// 这点轮询开销换来一个能被测试驱动完的实现。
const inflightPollInterval = 20 * time.Millisecond

// inflight 统计仍未返回的 handler 条数。
type inflight struct {
	n atomic.Int64
}

// wrap 把计数贴在 handler 两侧。
//
// 装在最外层而非 pipeline 内部：要等的是「handler 还没返回」，而每请求的
// 上报队列是 Serve 的 defer 排空的——等到 handler 返回就等到了上报投完，
// 无需把那个队列暴露到这一层。
func (i *inflight) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i.n.Add(1)
		// defer 而非尾部直接减：handler panic 时也必须归零，
		// 否则一次 panic 会让此后每次关停都白等满整个 Linger。
		defer i.n.Add(-1)
		next.ServeHTTP(w, r)
	})
}

// wait 等在途归零，返回放弃时仍在途的条数。
func (i *inflight) wait(ctx context.Context) int64 {
	ticker := time.NewTicker(inflightPollInterval)
	defer ticker.Stop()
	for {
		// 先查再等：已经是零时不该付出一个轮询周期的代价。
		if n := i.n.Load(); n == 0 {
			return 0
		}
		select {
		case <-ctx.Done():
			return i.n.Load()
		case <-ticker.C:
		}
	}
}
