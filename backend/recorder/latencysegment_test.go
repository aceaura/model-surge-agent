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

// 两段要一路走到实时环里。
//
// 这一层是纯字段搬运，漏掉一个字段没有任何编译或运行错误，
// 症状只是「实时页上那两列永远是空的」——而前几轮已经在这类纯搬运层
// 上漏过两次，所以它必须有自己的断言。
func TestLiveEntryCarriesLatencySegments(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR not set")
	}
	c := cache.New(cache.Options{Addr: addr, DB: 11, TTL: time.Minute})
	t.Cleanup(c.Close)
	ctx := context.Background()
	if err := c.Ping(ctx); err != nil {
		t.Skipf("redis unreachable: %v", err)
	}

	r := &Recorder{Cache: c}
	// 两段给不同的值：搬运时互换也要能被抓到。
	r.Record(pipeline.Record{
		RequestID:       "req-seg",
		At:              time.Now().UTC(),
		InboundProtocol: "anthropic",
		UserModel:       "user-model",
		Outcome:         relayclient.OutcomeNormal,
		LatencyMS:       900,
		DispatchMS:      111,
		UpstreamMS:      222,
	})

	for _, e := range c.Live(ctx, 50) {
		if e.RequestID != "req-seg" {
			continue
		}
		if e.DispatchMS != 111 {
			t.Errorf("live dispatch_ms = %d，要 111（读到 222 说明两段搬反了）",
				e.DispatchMS)
		}
		if e.UpstreamMS != 222 {
			t.Errorf("live upstream_ms = %d，要 222（读到 111 说明两段搬反了）",
				e.UpstreamMS)
		}
		return
	}
	t.Fatal("实时环里找不到这条：两段的搬运路径根本没跑到")
}
