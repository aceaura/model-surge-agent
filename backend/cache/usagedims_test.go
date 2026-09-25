package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/relayclient"
)

// 本轮的入口证据：明细表（store.RequestLog）已有五维 usage，而这里的摘要与
// 分钟桶只有两维。少报的正好是计费权重最偏的三维（缓存写通常 1.25×、
// 缓存读 0.1×、推理计入输出），于是 /admin/requests 与 /admin/live、
// /admin/stats 长期对不上，而两侧都不报错。
func TestLiveEntryCarriesEveryUsageDimension(t *testing.T) {
	c := open(t)
	ctx := context.Background()
	want := LiveEntry{
		RequestID:        "r-usage",
		At:               time.Now().UTC().Truncate(time.Second),
		InboundProtocol:  "anthropic",
		Outcome:          "normal",
		InputTokens:      11,
		OutputTokens:     22,
		CacheReadTokens:  33,
		CacheWriteTokens: 44,
		ReasoningTokens:  55,
		CacheWrite5mTokens: 66,
		CacheWrite1hTokens: 77,
	}
	c.PushLive(ctx, want)

	got := c.Live(ctx, 10)
	if len(got) != 1 {
		t.Fatalf("摘要条数 = %d，想要 1", len(got))
	}
	if got[0].CacheReadTokens != 33 || got[0].CacheWriteTokens != 44 ||
		got[0].ReasoningTokens != 55 {
		t.Errorf("三维往返丢了：读 %d 写 %d 推理 %d，想要 33/44/55",
			got[0].CacheReadTokens, got[0].CacheWriteTokens, got[0].ReasoningTokens)
	}
	if got[0].CacheWrite5mTokens != 66 || got[0].CacheWrite1hTokens != 77 {
		t.Errorf("TTL 明细两维往返丢了：5m %d 1h %d，想要 66/77",
			got[0].CacheWrite5mTokens, got[0].CacheWrite1hTokens)
	}
	if got[0].InputTokens != 11 || got[0].OutputTokens != 22 {
		t.Errorf("原有两维被改坏了：入 %d 出 %d", got[0].InputTokens, got[0].OutputTokens)
	}
}

// 分钟桶同理。累加而非覆盖：同一分钟内多条请求要叠起来。
func TestBucketAccumulatesEveryUsageDimension(t *testing.T) {
	c := open(t)
	ctx := context.Background()
	now := time.Now().UTC()
	c.Incr(ctx, now, "normal", relayclient.Usage{
		InputTokens: 10, OutputTokens: 20,
		CacheReadTokens: 30, CacheWriteTokens: 40, ReasoningTokens: 50,
		CacheWrite5mTokens: 60, CacheWrite1hTokens: 70,
	}, 100)
	c.Incr(ctx, now, "normal", relayclient.Usage{
		InputTokens: 1, OutputTokens: 2,
		CacheReadTokens: 3, CacheWriteTokens: 4, ReasoningTokens: 5,
		CacheWrite5mTokens: 6, CacheWrite1hTokens: 7,
	}, 10)

	buckets := c.Buckets(ctx, now, time.Minute)
	if len(buckets) != 1 {
		t.Fatalf("桶数 = %d，想要 1", len(buckets))
	}
	b := buckets[0]
	for _, tc := range []struct {
		name string
		got  int64
		want int64
	}{
		{"input", b.InputTokens, 11},
		{"output", b.OutputTokens, 22},
		{"cache_read", b.CacheReadTokens, 33},
		{"cache_write", b.CacheWriteTokens, 44},
		{"reasoning", b.ReasoningTokens, 55},
		{"cache_write_5m", b.CacheWrite5mTokens, 66},
		{"cache_write_1h", b.CacheWrite1hTokens, 77},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d，想要 %d", tc.name, tc.got, tc.want)
		}
	}
}

// 旧版本写进 Redis 的摘要没有新字段，升级后还要能读回来。
// liveMaxLen 条摘要与 TTL 内的桶都会跨越一次发版——整条丢弃等于实时页
// 在升级后空一段时间，而那正是最需要看它的时候。
//
// 直接构造缺字段的 JSON 而不是「用旧结构体序列化」：旧结构体改完就没了，
// 拿它当基准的断言下一轮就失效。
func TestSummaryWithoutNewFieldsStillDecodes(t *testing.T) {
	raw := `{"request_id":"old","inbound_protocol":"anthropic","outcome":"normal",` +
		`"input_tokens":7,"output_tokens":8}`
	var e LiveEntry
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		t.Fatalf("旧摘要解不出来: %v", err)
	}
	if e.RequestID != "old" || e.InputTokens != 7 || e.OutputTokens != 8 {
		t.Errorf("旧字段读错了: %+v", e)
	}
	if e.CacheReadTokens != 0 || e.CacheWriteTokens != 0 || e.ReasoningTokens != 0 {
		t.Errorf("缺失的新字段该读作零，实际 %d/%d/%d",
			e.CacheReadTokens, e.CacheWriteTokens, e.ReasoningTokens)
	}
	if e.CacheWrite5mTokens != 0 || e.CacheWrite1hTokens != 0 {
		t.Errorf("缺失的 TTL 明细该读作零，实际 %d/%d",
			e.CacheWrite5mTokens, e.CacheWrite1hTokens)
	}
	// 三态的零值必须是「未知」而不是「落库失败」。
	if e.LogPersisted != nil {
		t.Errorf("旧摘要没有落库标记，该是 nil，实际 %v", *e.LogPersisted)
	}
}

// 桶的新字段不需要迁移：HIncrBy 对缺字段按零起算，bucketFrom 也只认它见到的键。
// 这条钉住这件事，否则下一个人会以为得写一次 Redis 迁移。
func TestBucketWithoutNewFieldsReadsAsZero(t *testing.T) {
	c := open(t)
	ctx := context.Background()
	now := time.Now().UTC()
	// 只写旧三键，模拟旧版本留下的桶。
	key := fmt.Sprintf(keyStatFmt, now.Format(statMinute))
	if err := c.rdb.HSet(ctx, key, fieldTotal, 1, fieldInput, 9, fieldOutput, 8).Err(); err != nil {
		t.Fatalf("hset: %v", err)
	}
	buckets := c.Buckets(ctx, now, time.Minute)
	if len(buckets) != 1 {
		t.Fatalf("桶数 = %d，想要 1", len(buckets))
	}
	b := buckets[0]
	if b.InputTokens != 9 || b.OutputTokens != 8 {
		t.Errorf("旧键读错了: 入 %d 出 %d", b.InputTokens, b.OutputTokens)
	}
	if b.CacheReadTokens != 0 || b.CacheWriteTokens != 0 || b.ReasoningTokens != 0 {
		t.Errorf("缺失的新键该读作零，实际 %d/%d/%d",
			b.CacheReadTokens, b.CacheWriteTokens, b.ReasoningTokens)
	}
	if b.CacheWrite5mTokens != 0 || b.CacheWrite1hTokens != 0 {
		t.Errorf("缺失的 TTL 明细键该读作零，实际 %d/%d",
			b.CacheWrite5mTokens, b.CacheWrite1hTokens)
	}
}
