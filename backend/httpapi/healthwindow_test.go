package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/relayclient"
)

// 窗口内的重复命中只探一次。
//
// 这个端点免鉴权，容器探针、反代、监控可能同时盯着它，而每次探测要做
// 一次 PG Ping、一次真实的调度层 HTTP 调用、两个 report_outbox 上的全表
// count。最后那一项在队列积压时最贵，而队列积压恰恰是探针被看得最紧的
// 时候——不加窗口就是自我放大。
func TestHealthWithinWindowProbesOnce(t *testing.T) {
	var hits atomic.Int64
	relay := readyServer(t, &hits)
	now := time.Now()
	c := &Checker{Relay: relay, Window: time.Second,
		Now: func() time.Time { return now }}

	for i := 0; i < 5; i++ {
		c.Check(context.Background())
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("探了 %d 次，want 1——窗口没生效", got)
	}
}

// 并发命中也只探一次。
func TestConcurrentHealthChecksProbeOnce(t *testing.T) {
	var hits atomic.Int64
	// 慢一点，确保后到的命中真的落在合并窗口里。
	relay := slowReadyServer(t, &hits, 60*time.Millisecond)
	c := &Checker{Relay: relay, Window: time.Minute}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Check(context.Background())
		}()
	}
	wg.Wait()

	if got := hits.Load(); got != 1 {
		t.Errorf("并发命中探了 %d 次，want 1", got)
	}
}

// 窗口过后必须重新探：否则运维看到的是一个永久陈旧的状态。
func TestHealthReprobesAfterWindow(t *testing.T) {
	var hits atomic.Int64
	relay := readyServer(t, &hits)
	now := time.Now()
	c := &Checker{Relay: relay, Window: time.Second,
		Now: func() time.Time { return now }}

	c.Check(context.Background())
	now = now.Add(2 * time.Second)
	c.Check(context.Background())

	if got := hits.Load(); got != 2 {
		t.Errorf("探了 %d 次，want 2——窗口过后没解封", got)
	}
}

// 依赖状态变化必须在一个窗口之后反映出来。
func TestHealthReflectsDependencyChangeAfterWindow(t *testing.T) {
	var down atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		writeModels(w)
	}))
	t.Cleanup(srv.Close)

	now := time.Now()
	c := &Checker{Relay: relayclient.New(srv.URL, ""), Window: time.Second,
		Now: func() time.Time { return now }}

	if h := c.Check(context.Background()); h.Relay != "ok" {
		t.Fatalf("relay = %q, want ok", h.Relay)
	}
	down.Store(true)
	// 仍在窗口内：读到的是快照，这是刻意的。
	if h := c.Check(context.Background()); h.Relay != "ok" {
		t.Errorf("窗口内 relay = %q，快照没被复用", h.Relay)
	}
	now = now.Add(2 * time.Second)
	if h := c.Check(context.Background()); h.Relay == "ok" {
		t.Error("窗口过后仍报 relay ok——快照永久陈旧了")
	}
}

// 发起方取消不得让这次探测的结果作废。
//
// 探测结果要给窗口内所有命中用。用发起者自己的 ctx 派生的话，第一个探针
// 超时断开就会让后面那一批也拿不到结果，于是它们各自再探一次——窗口在
// 最需要它的时候失效。
func TestHealthProbeSurvivesCallerCancellation(t *testing.T) {
	started := make(chan struct{})
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-started:
		default:
			close(started)
		}
		time.Sleep(120 * time.Millisecond)
		writeModels(w)
	}))
	t.Cleanup(srv.Close)

	c := &Checker{Relay: relayclient.New(srv.URL, ""), Window: time.Minute}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan Health, 1)
	go func() { done <- c.Check(ctx) }()
	<-started
	cancel()

	if h := <-done; h.Relay != "ok" {
		t.Errorf("relay = %q，发起方取消把探测结果作废了", h.Relay)
	}
	// 快照要留下，否则下一次命中又要探一遍。
	if h := c.Check(context.Background()); h.Relay != "ok" {
		t.Errorf("快照没存下来: relay = %q", h.Relay)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("探了 %d 次，want 1", got)
	}
}

// 窗口零值必须兜底成内置值。
//
// 不兜底的话零窗口等于没有窗口，而 Checker 的零值正是生产装配用的形态
// （cmd/server 不设 Window）——于是这一整条治理在生产里是关着的，
// 而所有显式设了窗口的测试全绿。
func TestZeroWindowFallsBackToDefault(t *testing.T) {
	var hits atomic.Int64
	relay := readyServer(t, &hits)
	c := &Checker{Relay: relay}

	for i := 0; i < 4; i++ {
		c.Check(context.Background())
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("零窗口下探了 %d 次，want 1——默认窗口没兜底，"+
			"而生产装配用的就是这个零值", got)
	}
}

func readyServer(t *testing.T, hits *atomic.Int64) *relayclient.Client {
	t.Helper()
	return slowReadyServer(t, hits, 0)
}

func slowReadyServer(t *testing.T, hits *atomic.Int64, delay time.Duration) *relayclient.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if delay > 0 {
			time.Sleep(delay)
		}
		writeModels(w)
	}))
	t.Cleanup(srv.Close)
	return relayclient.New(srv.URL, "")
}
