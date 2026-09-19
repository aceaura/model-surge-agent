package httpapi

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/cache"
	"github.com/redis/go-redis/v9"
)

// 回源成功必须把结果写进共享缓存。
//
// 不写的话合并只在那一瞬间有效：这一组请求共享了一次回源，下一组进来
// 又是一次满额回源。TTL 到期后的尖峰被压成了每批一次而不是每周期一次，
// 而清单几分钟才变一次——那些回源全是白跑的。
func TestSuccessfulFetchPopulatesTheSharedCache(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR not set")
	}
	rdb := cache.New(cache.Options{Addr: addr, TTL: time.Minute})
	t.Cleanup(rdb.Close)
	ctx := context.Background()
	if err := rdb.Ping(ctx); err != nil {
		t.Skipf("redis unavailable: %v", err)
	}
	// 真删键，否则第一次调用就命中，测不到写入。不能用 PutModels(nil)：
	// 那会写进一个 JSON null，读回来照样算命中。
	raw := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = raw.Close() })
	if err := raw.Del(ctx, "msa:models").Err(); err != nil {
		t.Fatalf("del: %v", err)
	}

	var hits atomic.Int64
	m := &CachedModels{Relay: modelsServer(t, &hits, 0), Cache: rdb}

	if _, err := m.List(ctx); err != nil {
		t.Fatalf("List: %v", err)
	}
	// 第二次应当直接命中缓存。
	if _, err := m.List(ctx); err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("回源了 %d 次，want 1——回源结果没写进共享缓存", got)
	}
}
