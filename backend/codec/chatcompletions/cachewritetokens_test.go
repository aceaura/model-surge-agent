package chatcompletions

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次40：缓存写入量的官方位置是 prompt_tokens_details.cache_write_tokens
//（与 anthropic 的 cache_creation_input_tokens、responses 的
// input_tokens_details.cache_write_tokens 同一位）。此前 chat 解码只读两个
// 顶层兼容别名，从不读官方嵌套位——上游照官方形状给出时这份对账凭证被静默
// 丢弃。这组测试钉住：解码官方嵌套位优先、别名回落、编码双位置回写。

// 官方嵌套位单独给出时进 IR.CacheWriteTokens。
func TestCacheWriteTokensDecodeNestedOfficial(t *testing.T) {
	got := convertUsage(wireUsage{
		PromptTokens:     100,
		CompletionTokens: 50,
		PromptTokensDetails: &wirePromptDetails{
			CachedTokens:     10,
			CacheWriteTokens: 33,
		},
	})
	if got.CacheWriteTokens != 33 {
		t.Fatalf("官方嵌套位的缓存写入量没进 IR：%+v", got)
	}
	if got.CacheReadTokens != 10 {
		t.Errorf("同对象的 cached_tokens 被改坏：%d", got.CacheReadTokens)
	}
}

// 官方嵌套位与顶层别名同时给出时，嵌套位优先（口径更权威）。
func TestCacheWriteTokensNestedPriorityOverAlias(t *testing.T) {
	got := convertUsage(wireUsage{
		PromptTokensDetails: &wirePromptDetails{CacheWriteTokens: 33},
		CacheWriteTokens:    11,
		CacheCreationTokens: 22,
	})
	if got.CacheWriteTokens != 33 {
		t.Fatalf("官方嵌套位该优先于别名，实得：%d", got.CacheWriteTokens)
	}
}

// 官方嵌套位缺席时回落到顶层 cache_write_tokens 别名。
func TestCacheWriteTokensAliasFallback(t *testing.T) {
	got := convertUsage(wireUsage{CacheWriteTokens: 11})
	if got.CacheWriteTokens != 11 {
		t.Fatalf("顶层别名 cache_write_tokens 没被回落读到：%d", got.CacheWriteTokens)
	}
}

// 两个别名都给时，cache_write_tokens 先于 cache_creation_tokens。
func TestCacheWriteTokensAliasOrder(t *testing.T) {
	got := convertUsage(wireUsage{CacheWriteTokens: 11, CacheCreationTokens: 22})
	if got.CacheWriteTokens != 11 {
		t.Fatalf("别名优先级错了，实得：%d", got.CacheWriteTokens)
	}
	got2 := convertUsage(wireUsage{CacheCreationTokens: 22})
	if got2.CacheWriteTokens != 22 {
		t.Fatalf("cache_creation_tokens 别名没被读到：%d", got2.CacheWriteTokens)
	}
}

// 编码侧把缓存写入量同时写到官方嵌套位与顶层别名，二者同值。
func TestCacheWriteTokensEncodeWritesBothPositions(t *testing.T) {
	w := renderUsage(ir.Usage{InputTokens: 5, OutputTokens: 7, CacheWriteTokens: 33})
	if w.PromptTokensDetails == nil {
		t.Fatalf("有缓存写入量却没写出 prompt_tokens_details 对象")
	}
	if w.PromptTokensDetails.CacheWriteTokens != 33 {
		t.Errorf("官方嵌套位没写对：%+v", w.PromptTokensDetails)
	}
	if w.CacheWriteTokens != 33 {
		t.Errorf("顶层别名没写对：%d", w.CacheWriteTokens)
	}
}

// 缓存写入量为零时不凭空造嵌套明细里的该键（omitempty）。
func TestCacheWriteTokensZeroStaysAbsent(t *testing.T) {
	w := renderUsage(ir.Usage{InputTokens: 5, OutputTokens: 7})
	if w.CacheWriteTokens != 0 {
		t.Errorf("零值不该写顶层别名：%d", w.CacheWriteTokens)
	}
	if w.PromptTokensDetails != nil && w.PromptTokensDetails.CacheWriteTokens != 0 {
		t.Errorf("零值不该写嵌套位：%+v", w.PromptTokensDetails)
	}
}

// 端到端：上游非流式响应里官方嵌套位的缓存写入量，解码进 IR、编码出站可见。
func TestCacheWriteTokensResponseRoundTrip(t *testing.T) {
	body := []byte(`{"id":"c1","object":"chat.completion","created":1700000000,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,` +
		`"prompt_tokens_details":{"cached_tokens":10,"cache_write_tokens":33}}}`)
	resp, _, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if resp.Usage.CacheWriteTokens != 33 {
		t.Fatalf("官方嵌套位的缓存写入量没进 IR：%+v", resp.Usage)
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	// 出站两处都该带上 33：嵌套位与顶层别名。
	if n := strings.Count(string(out), `"cache_write_tokens":33`); n != 2 {
		t.Errorf("出站该在嵌套位与顶层别名各写一次 cache_write_tokens:33，实得 %d 次：%s", n, out)
	}
}
