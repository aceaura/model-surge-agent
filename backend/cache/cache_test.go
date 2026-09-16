package cache

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/relayclient"
)

// 缓存里没有任何东西是必需的，所以这里测两件事：接得上时读写是对的，
// 接不上时全部降级而非报错。后者用 nil 与坏地址两种方式覆盖，
// 因为部署上「没配 Redis」与「配了但连不上」都会发生。

// open 连测试 Redis；未配置则跳过（与同族两个仓库同惯例）。
func open(t *testing.T) *Cache {
	t.Helper()
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR not set")
	}
	c := New(Options{Addr: addr, DB: 9, TTL: time.Minute})
	t.Cleanup(c.Close)
	ctx := context.Background()
	if err := c.Ping(ctx); err != nil {
		t.Skipf("redis unreachable: %v", err)
	}
	if err := c.rdb.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return c
}

func TestNilCacheDegradesInsteadOfPanicking(t *testing.T) {
	// 未配 Redis 是合法部署形态，New 返回 nil，全部方法必须能安全调用。
	var c *Cache
	ctx := context.Background()

	if _, ok := c.Models(ctx); ok {
		t.Error("a nil cache must never report a hit")
	}
	c.PutModels(ctx, []relayclient.UserModelSummary{{Name: "m"}})
	c.PushLive(ctx, LiveEntry{RequestID: "r"})
	c.Incr(ctx, time.Now(), "normal", 1, 2, 3)
	if got := c.Live(ctx, 10); got != nil {
		t.Errorf("live = %+v, want nil", got)
	}
	if got := c.Buckets(ctx, time.Now(), time.Hour); got != nil {
		t.Errorf("buckets = %+v, want nil", got)
	}
	if c.Ping(ctx) == nil {
		t.Error("a nil cache must report itself unavailable")
	}
}

func TestUnreachableRedisDegradesInsteadOfFailing(t *testing.T) {
	// 配了但连不上：读当作 miss，写静默丢弃。数据面不该因此受影响。
	c := New(Options{Addr: "127.0.0.1:1", TTL: time.Minute})
	t.Cleanup(c.Close)
	ctx := context.Background()

	if c.Ping(ctx) == nil {
		t.Fatal("ping must fail against an unreachable address")
	}
	if _, ok := c.Models(ctx); ok {
		t.Error("an unreachable cache must report a miss so the caller falls back")
	}
	c.PutModels(ctx, []relayclient.UserModelSummary{{Name: "m"}})
	c.PushLive(ctx, LiveEntry{RequestID: "r"})
	c.Incr(ctx, time.Now(), "normal", 1, 2, 3)
	if got := c.Live(ctx, 10); len(got) != 0 {
		t.Errorf("live = %+v, want empty", got)
	}
	if got := c.Buckets(ctx, time.Now(), time.Hour); len(got) != 0 {
		t.Errorf("buckets = %+v, want empty", got)
	}
}

func TestModelsRoundTrip(t *testing.T) {
	c := open(t)
	ctx := context.Background()

	if _, ok := c.Models(ctx); ok {
		t.Fatal("an empty cache must report a miss")
	}
	want := []relayclient.UserModelSummary{
		{Name: "user-model", Collection: "main", Protocol: "anthropic", Enabled: true},
	}
	c.PutModels(ctx, want)
	got, ok := c.Models(ctx)
	if !ok {
		t.Fatal("models must hit after a put")
	}
	if len(got) != 1 || got[0].Name != "user-model" || !got[0].Enabled {
		t.Errorf("models = %+v, want %+v", got, want)
	}
}

func TestCorruptCachedModelsAreTreatedAsAMiss(t *testing.T) {
	// 缓存里可能留着旧版本写的格式。当作没有并回源覆盖掉它，
	// 比报错让整个清单接口挂掉要好。
	c := open(t)
	ctx := context.Background()
	if err := c.rdb.Set(ctx, keyModels, "not json", time.Minute).Err(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, ok := c.Models(ctx); ok {
		t.Error("undecodable cached models must count as a miss")
	}
}

func TestLiveKeepsNewestFirstAndTrimsToTheCap(t *testing.T) {
	c := open(t)
	ctx := context.Background()

	for i := range liveMaxLen + 20 {
		c.PushLive(ctx, LiveEntry{RequestID: id(i), Outcome: "normal"})
	}
	got := c.Live(ctx, liveMaxLen)
	if len(got) != liveMaxLen {
		t.Fatalf("live length = %d, want it trimmed to %d", len(got), liveMaxLen)
	}
	// 最新在前：实时页从头读就是最近的流水。
	if got[0].RequestID != id(liveMaxLen+19) {
		t.Errorf("first entry = %q, want the newest %q", got[0].RequestID, id(liveMaxLen+19))
	}
}

func TestBucketsAggregateOutcomesAndTokens(t *testing.T) {
	c := open(t)
	ctx := context.Background()
	now := time.Now().UTC()

	c.Incr(ctx, now, "normal", 100, 20, 300)
	c.Incr(ctx, now, "normal", 50, 10, 100)
	c.Incr(ctx, now, "abnormal", 0, 0, 50)

	buckets := c.Buckets(ctx, now, time.Hour)
	if len(buckets) != 1 {
		t.Fatalf("buckets = %d, want the single minute that has data", len(buckets))
	}
	b := buckets[0]
	if b.Total != 3 {
		t.Errorf("total = %d, want 3", b.Total)
	}
	if b.Outcomes["normal"] != 2 || b.Outcomes["abnormal"] != 1 {
		t.Errorf("outcomes = %v, want normal 2 / abnormal 1", b.Outcomes)
	}
	if b.InputTokens != 150 || b.OutputTokens != 30 {
		t.Errorf("tokens = %d/%d, want 150/30", b.InputTokens, b.OutputTokens)
	}
	if b.LatencySumMS != 450 {
		t.Errorf("latency sum = %d, want 450", b.LatencySumMS)
	}
}

func TestAnOutcomeNamedTotalDoesNotClobberTheCount(t *testing.T) {
	// outcome 的计数键带前缀，否则一个叫 total 的 outcome 会盖掉总数。
	c := open(t)
	ctx := context.Background()
	now := time.Now().UTC()

	c.Incr(ctx, now, "total", 0, 0, 0)
	buckets := c.Buckets(ctx, now, time.Hour)
	if len(buckets) != 1 {
		t.Fatalf("buckets = %d, want 1", len(buckets))
	}
	if buckets[0].Total != 1 {
		t.Errorf("total = %d, want 1 request counted", buckets[0].Total)
	}
	if buckets[0].Outcomes["total"] != 1 {
		t.Errorf("outcomes = %v, want the outcome recorded separately", buckets[0].Outcomes)
	}
}

func TestEmptyMinutesAreOmittedRatherThanZeroFilled(t *testing.T) {
	// 缺的分钟不补零，由前端决定怎么画空隙。
	c := open(t)
	ctx := context.Background()
	now := time.Now().UTC()

	c.Incr(ctx, now.Add(-30*time.Minute), "normal", 1, 1, 1)
	buckets := c.Buckets(ctx, now, time.Hour)
	if len(buckets) != 1 {
		t.Fatalf("buckets = %d, want only the minute with data", len(buckets))
	}
}

func id(i int) string { return "req-" + strconv.Itoa(i) }
