package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeServer 记录 Shutdown 收到的 context，并可被要求烧光它的预算。
type fakeServer struct {
	// burn 为真表示模拟「有流在途」：等到 ctx 到期才返回 DeadlineExceeded，
	// 与真 http.Server 在宽限期内等不到在途请求时的行为一致。
	burn     bool
	gotErr   error
	deadline time.Duration
	calls    int
}

func (f *fakeServer) Shutdown(ctx context.Context) error {
	f.calls++
	if dl, ok := ctx.Deadline(); ok {
		f.deadline = time.Until(dl)
	}
	if !f.burn {
		return nil
	}
	<-ctx.Done()
	f.gotErr = ctx.Err()
	return ctx.Err()
}

// fakeFlusher 记录 Drain 拿到的 context 在进入那一刻是否还活着。
type fakeFlusher struct {
	calls    int
	ctxErr   error
	deadline time.Duration
	sent     int
}

func (f *fakeFlusher) Drain(ctx context.Context) int {
	f.calls++
	// 在进入的那一刻取，而不是留着 ctx 之后再看：留着看会把本测试自己
	// 的耗时算进去，而要守的是「进来时它还能用」。
	f.ctxErr = ctx.Err()
	if dl, ok := ctx.Deadline(); ok {
		f.deadline = time.Until(dl)
	}
	return f.sent
}

// fakeWaiter 记录被调次数与拿到的预算，并回报一个固定的剩余量。
type fakeWaiter struct {
	calls    int
	left     int64
	deadline time.Duration
}

func (f *fakeWaiter) wait(ctx context.Context) int64 {
	f.calls++
	if dl, ok := ctx.Deadline(); ok {
		f.deadline = time.Until(dl)
	}
	return f.left
}

// 短预算：测试要在毫秒级跑完，而 shutdown 只关心相对关系不关心绝对值。
var testPlan = shutdownPlan{
	Grace:  60 * time.Millisecond,
	Linger: 40 * time.Millisecond,
	Flush:  200 * time.Millisecond,
}

// 冲队列阶段必须拿到一份还活着的 context，即使宽限期已被烧光。
//
// 这是本轮修的核心缺陷的回归锁：此前三个阶段共用一个 context，
// 只要有流在途，Shutdown 就会烧光它，Drain 随后拿到一个已过期的 context，
// Queue.Due 立刻失败——那次「退出前冲一次队列」在它唯一存在意义的场景里
// 保证是空操作。
func TestFlushGetsALiveContextAfterGraceExpires(t *testing.T) {
	srv := &fakeServer{burn: true}
	flusher := &fakeFlusher{}
	shutdown(quietLog(), srv, &fakeWaiter{}, flusher, testPlan)

	if !errors.Is(srv.gotErr, context.DeadlineExceeded) {
		t.Fatalf("宽限期没被烧光，这个装置测不到目标场景: %v", srv.gotErr)
	}
	if flusher.calls != 1 {
		t.Fatalf("Drain 被调 %d 次, want 1", flusher.calls)
	}
	if flusher.ctxErr != nil {
		t.Errorf("Drain 拿到的 context 已过期: %v——冲队列变成空操作", flusher.ctxErr)
	}
}

// 各阶段的预算必须取自 plan，而不是写死在关停逻辑里。
func TestEachPhaseUsesItsOwnBudget(t *testing.T) {
	srv := &fakeServer{burn: true}
	waiter := &fakeWaiter{left: 2}
	flusher := &fakeFlusher{}
	plan := shutdownPlan{
		Grace:  80 * time.Millisecond,
		Linger: 300 * time.Millisecond,
		Flush:  2 * time.Second,
	}
	shutdown(quietLog(), srv, waiter, flusher, plan)

	// 判区间而不是判相等：从 WithTimeout 到读 Deadline 之间总有耗时。
	// 三个预算刻意取得彼此相差数倍，于是「拿错了别人的那份」一定落在区间外。
	assertBudget(t, "grace", srv.deadline, plan.Grace)
	assertBudget(t, "linger", waiter.deadline, plan.Linger)
	assertBudget(t, "flush", flusher.deadline, plan.Flush)
}

func assertBudget(t *testing.T, name string, got, want time.Duration) {
	t.Helper()
	if got > want || got < want/2 {
		t.Errorf("%s 预算 = %v, want 约 %v", name, got, want)
	}
}

// Shutdown 干净返回时不该再等在途：在途已清零，再等就是白拖部署窗口。
func TestCleanShutdownSkipsTheLingerPhase(t *testing.T) {
	srv := &fakeServer{}
	waiter := &fakeWaiter{}
	flusher := &fakeFlusher{}
	shutdown(quietLog(), srv, waiter, flusher, testPlan)

	if waiter.calls != 0 {
		t.Errorf("干净关停后仍等了在途 %d 次", waiter.calls)
	}
	if flusher.calls != 1 {
		t.Errorf("Drain 被调 %d 次, want 1", flusher.calls)
	}
}

// Shutdown 超时时必须进入清理等待。
func TestTimedOutShutdownEntersTheLingerPhase(t *testing.T) {
	srv := &fakeServer{burn: true}
	waiter := &fakeWaiter{}
	shutdown(quietLog(), srv, waiter, &fakeFlusher{}, testPlan)

	if waiter.calls != 1 {
		t.Errorf("在途等待被调 %d 次, want 1", waiter.calls)
	}
}

// 三阶段顺序固定：先等流收尾，再等在途，最后冲队列。
//
// 顺序不能反：冲队列排在等在途之前的话，正在收尾的那些请求的上报还没入队，
// 这一冲就冲不到它们，而它们恰恰是关停期最可能丢的那批。
func TestPhaseOrder(t *testing.T) {
	var order []string
	var mu sync.Mutex
	note := func(s string) {
		mu.Lock()
		order = append(order, s)
		mu.Unlock()
	}

	srv := &orderedServer{note: note}
	waiter := &orderedWaiter{note: note}
	flusher := &orderedFlusher{note: note}
	shutdown(quietLog(), srv, waiter, flusher, testPlan)

	want := []string{"shutdown", "wait", "drain"}
	if len(order) != len(want) {
		t.Fatalf("阶段序列 = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("阶段序列 = %v, want %v", order, want)
		}
	}
}

type orderedServer struct{ note func(string) }

func (o *orderedServer) Shutdown(ctx context.Context) error {
	o.note("shutdown")
	return errors.New("still busy")
}

type orderedWaiter struct{ note func(string) }

func (o *orderedWaiter) wait(ctx context.Context) int64 { o.note("wait"); return 0 }

type orderedFlusher struct{ note func(string) }

func (o *orderedFlusher) Drain(ctx context.Context) int { o.note("drain"); return 0 }

// 前两阶段都失败时冲队列仍要执行一次：它与那两阶段的成败无关。
func TestFlushRunsEvenWhenEverythingElseFailed(t *testing.T) {
	flusher := &fakeFlusher{sent: 3}
	shutdown(quietLog(), &fakeServer{burn: true}, &fakeWaiter{left: 5}, flusher, testPlan)

	if flusher.calls != 1 {
		t.Errorf("Drain 被调 %d 次, want 1", flusher.calls)
	}
}

// ---- inflight ----

// handler 执行期间计数为 1，返回后归零。
func TestInflightCountsAroundHandler(t *testing.T) {
	var tracker inflight
	inside := make(chan int64, 1)
	release := make(chan struct{})

	h := tracker.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inside <- tracker.n.Load()
		<-release
	}))

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}()

	if got := <-inside; got != 1 {
		t.Errorf("handler 执行期间在途 = %d, want 1", got)
	}
	close(release)
	<-done
	if got := tracker.n.Load(); got != 0 {
		t.Errorf("handler 返回后在途 = %d, want 0", got)
	}
}

// handler panic 也必须归零：一次 panic 让计数永久偏高的话，
// 此后每次关停都会白等满整个 Linger。
func TestInflightZeroesOnPanic(t *testing.T) {
	var tracker inflight
	h := tracker.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))

	func() {
		defer func() { _ = recover() }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}()

	if got := tracker.n.Load(); got != 0 {
		t.Errorf("panic 后在途 = %d, want 0", got)
	}
}

// 已经是零时立刻返回，不付一个轮询周期的代价。
func TestInflightWaitReturnsImmediatelyWhenIdle(t *testing.T) {
	var tracker inflight
	// 预算远小于一个轮询周期：先等再查的实现会在这里返回非零。
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	if got := tracker.wait(ctx); got != 0 {
		t.Errorf("空闲时 wait = %d, want 0", got)
	}
}

// 在途归零后立刻返回，而不是等满整个预算。
func TestInflightWaitReturnsWhenCountDrops(t *testing.T) {
	var tracker inflight
	tracker.n.Add(1)
	go func() {
		time.Sleep(2 * inflightPollInterval)
		tracker.n.Add(-1)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if got := tracker.wait(ctx); got != 0 {
		t.Fatalf("归零后 wait = %d, want 0", got)
	}
	// 上限取预算的一个小零头：等满预算才返回的实现会远远超出。
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("归零后 wait 耗时 %v，像是等满了预算", elapsed)
	}
}

// 等不到归零时返回仍在途的条数——运维靠这个数字知道这次部署丢了多少上报。
func TestInflightWaitReportsWhatIsLeft(t *testing.T) {
	var tracker inflight
	tracker.n.Add(3)
	ctx, cancel := context.WithTimeout(context.Background(), 2*inflightPollInterval)
	defer cancel()
	if got := tracker.wait(ctx); got != 3 {
		t.Errorf("放弃时 wait = %d, want 3", got)
	}
}

// wrap 必须真的把请求交给下一层，而不是把计数当成终点。
func TestInflightWrapDelegates(t *testing.T) {
	var tracker inflight
	hit := false
	h := tracker.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusTeapot)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if !hit {
		t.Error("包装后的 handler 没被调用")
	}
	if rec.Code != http.StatusTeapot {
		t.Errorf("状态码 = %d, want %d", rec.Code, http.StatusTeapot)
	}
}

// 计数必须能同时容纳多个在途请求。
func TestInflightCountsConcurrentRequests(t *testing.T) {
	var tracker inflight
	const n = 4
	entered := make(chan struct{}, n)
	release := make(chan struct{})
	h := tracker.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
	}))

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		}()
	}
	for i := 0; i < n; i++ {
		<-entered
	}
	if got := tracker.n.Load(); got != n {
		t.Errorf("在途 = %d, want %d", got, n)
	}
	close(release)
	wg.Wait()
	if got := tracker.n.Load(); got != 0 {
		t.Errorf("全部返回后在途 = %d, want 0", got)
	}
}

// *http.Server 必须满足关停用到的那一面：接口漂了要在编译期发现，
// 而不是等到装配那一行改出别的错误。
func TestHTTPServerSatisfiesShutdowner(t *testing.T) {
	var _ gracefulShutdowner = &http.Server{}
}

// 装配出来的监听器必须让请求先过在途计数。
//
// 漏掉这一层的后果完全不可见：请求照样服务、流水照样记，只有关停期的等待
// 永远看到零在途，于是被掐断的那批请求的上报静静丢掉。
func TestNewListenerWrapsHandlerWithInflightCounting(t *testing.T) {
	tracker := &inflight{}
	seen := make(chan int64, 1)
	l := newListener(":0", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- tracker.n.Load()
	}), tracker)

	l.Handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if got := <-seen; got != 1 {
		t.Errorf("handler 执行期间在途 = %d, want 1——装配漏了在途计数层", got)
	}
}

// 不设写超时：SSE 响应会持续数分钟，设了会把流从中间掐断。
func TestNewListenerLeavesWriteTimeoutUnset(t *testing.T) {
	l := newListener(":0", http.NewServeMux(), &inflight{})
	if l.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, 应当不设——它会把 SSE 流从中间掐断", l.WriteTimeout)
	}
	if l.ReadHeaderTimeout <= 0 {
		t.Error("ReadHeaderTimeout 未设，慢速发头的连接会一直占着")
	}
	if l.Addr != ":0" {
		t.Errorf("Addr = %q, want \":0\"", l.Addr)
	}
}
