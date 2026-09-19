package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/relayclient"
)

// 缓存 miss 时的并发回源必须合并成一次。
//
// TTL 一分钟，而清单页与每次列模型都走这里。不合并的话 TTL 到期那一瞬间
// 所有在列客户端一起 miss，调度层收到 N 倍尖峰；而它若正因此不健康，
// 每个请求都要各自等满自己的超时才失败，尖峰会持续整个超时窗口。
func TestConcurrentListCoalescesIntoOneFetch(t *testing.T) {
	var hits atomic.Int64
	// 让上游慢一点，确保后到的请求真的落在合并窗口里；不慢的话第一个
	// 请求可能在别人进来前就已经返回，这条用例会退化成顺序调用。
	relay := modelsServer(t, &hits, 60*time.Millisecond)
	m := &CachedModels{Relay: relay}

	const n = 12
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = m.List(context.Background())
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 个调用失败: %v", i, err)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("回源了 %d 次，want 1——%d 个并发请求没被合并", got, n)
	}
}

// 回源失败要在一个短窗口内被记住，窗口内不再撞墙。
func TestFailedFetchIsNegativelyCachedForAWindow(t *testing.T) {
	var hits atomic.Int64
	relay := failingModelsServer(t, &hits)
	now := time.Now()
	m := &CachedModels{Relay: relay, NegativeTTL: time.Second,
		Now: func() time.Time { return now }}

	if _, err := m.List(context.Background()); err == nil {
		t.Fatal("上游 500 却报成功")
	}
	if _, err := m.List(context.Background()); err == nil {
		t.Fatal("窗口内的第二次调用该直接拿到错误")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("回源了 %d 次，want 1——负缓存窗口没生效", got)
	}
}

// 窗口过后必须重新回源：调度层可能已经恢复了。
func TestNegativeCacheExpires(t *testing.T) {
	var hits atomic.Int64
	relay := failingModelsServer(t, &hits)
	now := time.Now()
	m := &CachedModels{Relay: relay, NegativeTTL: time.Second,
		Now: func() time.Time { return now }}

	if _, err := m.List(context.Background()); err == nil {
		t.Fatal("上游 500 却报成功")
	}
	now = now.Add(2 * time.Second)
	if _, err := m.List(context.Background()); err == nil {
		t.Fatal("窗口过后仍该去问一次")
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("回源了 %d 次，want 2——负缓存过期后没解封", got)
	}
}

// 发起方断开不得中断回源：这一次回源服务的是所有等待者与后来人。
func TestFetchSurvivesCallerCancellation(t *testing.T) {
	started := make(chan struct{})
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		close(started)
		// 在这段时间里发起方会取消自己的 ctx。
		time.Sleep(120 * time.Millisecond)
		writeModels(w)
	}))
	t.Cleanup(srv.Close)

	m := &CachedModels{Relay: relayclient.New(srv.URL, "")}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := m.List(ctx)
		done <- err
	}()
	<-started
	cancel()

	if err := <-done; err != nil {
		t.Errorf("发起方取消后回源被中断了: %v——用它自己的 ctx 派生就会这样", err)
	}
}

// 命中缓存的路径不得触发回源。
//
// 用一个永远命中的假缓存读不到，所以反过来测：第二次调用在 TTL 内应当
// 被负缓存或共享结果挡住，而这里没有 Redis，于是直接验「命中的语义」——
// 无 Redis 时每次都 miss，两次调用就该有两次回源。这条守的是
// 「不要把合并做成把结果永久钉住」。
func TestWithoutCacheEachCallStillRefetches(t *testing.T) {
	var hits atomic.Int64
	relay := modelsServer(t, &hits, 0)
	m := &CachedModels{Relay: relay}

	for i := 0; i < 2; i++ {
		if _, err := m.List(context.Background()); err != nil {
			t.Fatalf("List: %v", err)
		}
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("回源了 %d 次，want 2——顺序调用被错当成同一次合并", got)
	}
}

// Redis 未配置时合并仍要生效：合并是进程内的事。
func TestCoalescingWorksWithoutRedis(t *testing.T) {
	var hits atomic.Int64
	relay := modelsServer(t, &hits, 60*time.Millisecond)
	m := &CachedModels{Relay: relay, Cache: nil}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = m.List(context.Background())
		}()
	}
	wg.Wait()

	if got := hits.Load(); got != 1 {
		t.Errorf("无 Redis 时回源 %d 次，want 1", got)
	}
}

// 所有等待者拿到的是同一份结果，而不是各自的零值。
func TestWaitersGetTheSharedResult(t *testing.T) {
	var hits atomic.Int64
	relay := modelsServer(t, &hits, 60*time.Millisecond)
	m := &CachedModels{Relay: relay}

	var wg sync.WaitGroup
	got := make([][]relayclient.UserModelSummary, 6)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], _ = m.List(context.Background())
		}(i)
	}
	wg.Wait()

	for i, models := range got {
		if len(models) != 1 || models[0].Name != "kimi" {
			t.Errorf("第 %d 个等待者拿到 %+v，没拿到共享结果", i, models)
		}
	}
}

// modelsServer 起一个回固定清单的假调度层，delay 模拟回源耗时。
func modelsServer(t *testing.T, hits *atomic.Int64, delay time.Duration) *relayclient.Client {
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

func failingModelsServer(t *testing.T, hits *atomic.Int64) *relayclient.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, `{"error":{"code":"internal","message":"boom"}}`,
			http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return relayclient.New(srv.URL, "")
}

func writeModels(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(relayclient.ModelsResponse{
		Models: []relayclient.UserModelSummary{{Name: "kimi", Enabled: true}},
	})
}
