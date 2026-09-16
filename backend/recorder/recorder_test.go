package recorder

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/cache"
	"github.com/aceaura/model-surge-agent/backend/pipeline"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
)

func TestRecordSurvivesWithoutEitherBackend(t *testing.T) {
	// PG 与 Redis 都缺时也不能崩：数据面已经把响应交给客户端了，
	// 观测数据写不进去不该反过来影响那次调用。
	r := &Recorder{}
	r.Record(pipeline.Record{RequestID: "req-1", At: time.Now(), Outcome: "normal"})
}

func TestRecordReachesTheCacheEvenWithoutADatabase(t *testing.T) {
	// PG 挂了仍要能看实时页与趋势图：两条写入路径互不依赖。
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR not set")
	}
	c := cache.New(cache.Options{Addr: addr, DB: 10, TTL: time.Minute})
	t.Cleanup(c.Close)
	ctx := context.Background()
	if err := c.Ping(ctx); err != nil {
		t.Skipf("redis unreachable: %v", err)
	}

	// Log 为 nil 模拟数据库缺失。
	r := &Recorder{Cache: c}
	now := time.Now().UTC()
	r.Record(pipeline.Record{
		RequestID:       "req-cache-only",
		At:              now,
		InboundProtocol: "anthropic",
		UserModel:       "user-model",
		Outcome:         relayclient.OutcomeNormal,
		LatencyMS:       120,
		Usage:           relayclient.Usage{InputTokens: 10, OutputTokens: 5},
	})

	live := c.Live(ctx, 20)
	var found bool
	for _, e := range live {
		if e.RequestID == "req-cache-only" {
			found = true
			if e.Outcome != relayclient.OutcomeNormal || e.OutputTokens != 5 {
				t.Errorf("live entry = %+v, want the outcome and tokens carried", e)
			}
		}
	}
	if !found {
		t.Errorf("the record must reach the live ring: %+v", live)
	}

	buckets := c.Buckets(ctx, now, time.Hour)
	var counted int64
	for _, b := range buckets {
		counted += b.Outcomes[relayclient.OutcomeNormal]
	}
	if counted == 0 {
		t.Error("the record must be counted into a stat bucket")
	}
}
