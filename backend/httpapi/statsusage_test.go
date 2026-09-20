package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/cache"
	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/contract/agentv1"
	"github.com/aceaura/model-surge-agent/backend/pipeline"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
)

// /admin/stats 的桶投影与总计都必须带满五维。
//
// 这一层是独立的一次搬运：缓存桶里有五维不代表投影出去还有五维，而两侧都不
// 报错——趋势图少三维时看起来只是「这三条线一直贴着零」。
//
// 用真 Redis 而不是假缓存：Admin.Cache 是具体类型 *cache.Cache，注不进假实现。
func TestAdminStatsProjectsEveryUsageDimension(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR not set")
	}
	// DB 12 专供本文件：桶是按分钟聚合的，与别处共用会把别人的计数加进来。
	c := cache.New(cache.Options{Addr: addr, DB: 12, TTL: time.Minute})
	t.Cleanup(c.Close)
	ctx := context.Background()
	if err := c.Ping(ctx); err != nil {
		t.Skipf("redis unreachable: %v", err)
	}

	c.Incr(ctx, time.Now().UTC(), relayclient.OutcomeNormal, relayclient.Usage{
		InputTokens: 11, OutputTokens: 22,
		CacheReadTokens: 33, CacheWriteTokens: 44, ReasoningTokens: 55,
	}, 7)

	a := newAdmin(t)
	a.admin.Cache = c
	a.rebuild()
	w := a.get(t, "/admin/stats?window=1h", adminKey)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，想要 200；响应体 %s", w.Code, w.Body.String())
	}
	var got agentv1.Stats
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("解码: %v", err)
	}

	// 桶里找那一分钟：窗口里其余分钟是空桶。
	var found bool
	for _, b := range got.Buckets {
		if b.Total == 0 {
			continue
		}
		found = true
		for _, dim := range []struct {
			name string
			got  int64
			want int64
		}{
			{"cache_read_tokens", b.CacheReadTokens, 33},
			{"cache_write_tokens", b.CacheWriteTokens, 44},
			{"reasoning_tokens", b.ReasoningTokens, 55},
		} {
			// 用下界而不是相等：同一分钟内重跑会把计数叠加，
			// 而这组断言要钉的是「这一维有没有被搬过来」。
			if dim.got < dim.want {
				t.Errorf("桶的 %s = %d，至少该有 %d", dim.name, dim.got, dim.want)
			}
		}
	}
	if !found {
		t.Fatal("窗口里没有非空桶——投影或窗口解析坏了，这组断言等于没测")
	}

	for _, dim := range []struct {
		name string
		got  int64
		want int64
	}{
		{"cache_read_tokens", got.Totals.CacheReadTokens, 33},
		{"cache_write_tokens", got.Totals.CacheWriteTokens, 44},
		{"reasoning_tokens", got.Totals.ReasoningTokens, 55},
	} {
		if dim.got < dim.want {
			t.Errorf("总计的 %s = %d，至少该有 %d", dim.name, dim.got, dim.want)
		}
	}
}

// /admin/live 的摘要投影同理：它是与 stats 完全独立的另一次搬运。
func TestAdminLiveProjectsEveryUsageDimension(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR not set")
	}
	c := cache.New(cache.Options{Addr: addr, DB: 13, TTL: time.Minute})
	t.Cleanup(c.Close)
	ctx := context.Background()
	if err := c.Ping(ctx); err != nil {
		t.Skipf("redis unreachable: %v", err)
	}

	id := "live-five-dims"
	c.PushLive(ctx, cache.LiveEntry{
		RequestID: id, At: time.Now().UTC(), Outcome: relayclient.OutcomeNormal,
		InputTokens: 11, OutputTokens: 22,
		CacheReadTokens: 33, CacheWriteTokens: 44, ReasoningTokens: 55,
	})

	a := newAdmin(t)
	a.admin.Cache = c
	a.rebuild()
	w := a.get(t, "/admin/live", adminKey)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，想要 200；响应体 %s", w.Code, w.Body.String())
	}
	var page struct {
		Entries []agentv1.LiveEntry `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("解码: %v", err)
	}
	var got *agentv1.LiveEntry
	for i := range page.Entries {
		if page.Entries[i].RequestID == id {
			got = &page.Entries[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("响应里没有 %s；响应体 %s", id, w.Body.String())
	}
	if got.CacheReadTokens != 33 || got.CacheWriteTokens != 44 || got.ReasoningTokens != 55 {
		t.Errorf("摘要三维 = %d/%d/%d，想要 33/44/55（摘要按 id 取，不会叠加）",
			got.CacheReadTokens, got.CacheWriteTokens, got.ReasoningTokens)
	}
}

// /admin/requests 的摘要投影同样要带满五维。
//
// 这是第三次独立的搬运（另两处是 live 与 stats），而它是运维对账时最先看的
// 那个视图——明细表里五列俱全，投影少一维等于账本对得上而报表对不上。
func TestAdminRequestsProjectsEveryUsageDimension(t *testing.T) {
	a := newAdmin(t)
	a.requests.records = []pipeline.Record{{
		RequestID: "req-usage", At: time.Now().UTC(),
		InboundProtocol: codec.ProtocolAnthropic, Outcome: relayclient.OutcomeNormal,
		Usage: relayclient.Usage{
			InputTokens: 11, OutputTokens: 22,
			CacheReadTokens: 33, CacheWriteTokens: 44, ReasoningTokens: 55,
		},
	}}

	resp := a.get(t, "/admin/requests", adminKey)
	var page agentv1.RequestPage
	if err := json.Unmarshal(resp.Body.Bytes(), &page); err != nil {
		t.Fatalf("解码: %v: %s", err, resp.Body.String())
	}
	if len(page.Requests) != 1 {
		t.Fatalf("条数 = %d，想要 1", len(page.Requests))
	}
	got := page.Requests[0]
	for _, dim := range []struct {
		name string
		got  int64
		want int64
	}{
		{"input_tokens", got.InputTokens, 11},
		{"output_tokens", got.OutputTokens, 22},
		{"cache_read_tokens", got.CacheReadTokens, 33},
		{"cache_write_tokens", got.CacheWriteTokens, 44},
		{"reasoning_tokens", got.ReasoningTokens, 55},
	} {
		if dim.got != dim.want {
			t.Errorf("%s = %d，想要 %d", dim.name, dim.got, dim.want)
		}
	}
}
