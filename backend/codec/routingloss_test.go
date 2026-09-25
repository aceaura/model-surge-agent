package codec_test

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	_ "github.com/aceaura/model-surge-agent/backend/codec/anthropic"
	_ "github.com/aceaura/model-surge-agent/backend/codec/chatcompletions"
	_ "github.com/aceaura/model-surge-agent/backend/codec/gemini"
	_ "github.com/aceaura/model-surge-agent/backend/codec/responses"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// #23 路由参数两维：service_tier 按目标值集映射、prompt_cache_key 两系回写。
// 「无槽位」（gemini 的 tier、anthropic/gemini 的缓存键）与「有槽位但值集
// 装不下」（anthropic 收不了 priority、chat 收不了 ultrafast）是两种措辞：
// 前者读者认命，后者可以换档位值。缓存键值是客户端自选串，不回显。
// lossyFor 复用同包既有助手。

func TestMapServiceTier(t *testing.T) {
	cases := []struct {
		tier, proto, want string
		ok                bool
	}{
		{"auto", codec.ProtocolAnthropic, "auto", true},
		{"auto", codec.ProtocolChatCompletions, "auto", true},
		{"auto", codec.ProtocolResponses, "auto", true},
		{"standard_only", codec.ProtocolAnthropic, "standard_only", true},
		{"default", codec.ProtocolAnthropic, "standard_only", true},
		{"priority", codec.ProtocolAnthropic, "", false},
		{"flex", codec.ProtocolAnthropic, "", false},
		{"ultrafast", codec.ProtocolAnthropic, "", false},
		{"standard_only", codec.ProtocolChatCompletions, "default", true},
		{"ultrafast", codec.ProtocolChatCompletions, "", false},
		{"priority", codec.ProtocolChatCompletions, "priority", true},
		{"brand-new-tier", codec.ProtocolChatCompletions, "brand-new-tier", true},
		{"standard_only", codec.ProtocolResponses, "default", true},
		{"ultrafast", codec.ProtocolResponses, "ultrafast", true},
		{"flex", codec.ProtocolResponses, "flex", true},
		{"", codec.ProtocolAnthropic, "", true},
	}
	for _, c := range cases {
		got, ok := codec.MapServiceTier(c.tier, c.proto)
		if got != c.want || ok != c.ok {
			t.Errorf("MapServiceTier(%q, %s) = (%q, %v)，想要 (%q, %v)",
				c.tier, c.proto, got, ok, c.want, c.ok)
		}
	}
}

func TestServiceTierNotesPerTarget(t *testing.T) {
	// auto 三家全通；gemini 无槽位照实报。
	req := &ir.Request{Model: "m", MaxTokens: 16, ServiceTier: "auto"}
	for _, proto := range []string{codec.ProtocolAnthropic, codec.ProtocolChatCompletions, codec.ProtocolResponses} {
		if got := lossyFor(t, proto, req); strings.Contains(got, "service_tier") {
			t.Errorf("%s 有槽位且 auto 恒通，不该报：%q", proto, got)
		}
	}
	if got := lossyFor(t, codec.ProtocolGemini, req); !strings.Contains(got, "dropped service_tier") {
		t.Errorf("gemini 无槽位，应报丢弃：%q", got)
	}

	// 有槽位但值集装不下：措辞走「无等价档位」并回显档位值（官方枚举非敏感）。
	for _, c := range []struct{ tier, target string }{
		{"priority", codec.ProtocolAnthropic},
		{"flex", codec.ProtocolAnthropic},
		{"ultrafast", codec.ProtocolChatCompletions},
	} {
		bad := &ir.Request{Model: "m", MaxTokens: 16, ServiceTier: c.tier}
		got := lossyFor(t, c.target, bad)
		if !strings.Contains(got, "tier set has no equivalent") {
			t.Errorf("%s->%s 无等价档位未报告：%q", c.tier, c.target, got)
		}
		if !strings.Contains(got, `"`+c.tier+`"`) {
			t.Errorf("%s->%s 档位值应出现在措辞里：%q", c.tier, c.target, got)
		}
	}

	// 可映射的互译档位不报。
	for _, c := range []struct{ tier, target string }{
		{"default", codec.ProtocolAnthropic},
		{"standard_only", codec.ProtocolChatCompletions},
		{"standard_only", codec.ProtocolResponses},
		{"ultrafast", codec.ProtocolResponses},
		{"flex", codec.ProtocolResponses},
	} {
		ok := &ir.Request{Model: "m", MaxTokens: 16, ServiceTier: c.tier}
		if got := lossyFor(t, c.target, ok); strings.Contains(got, "service_tier") {
			t.Errorf("%s->%s 可映射，误报：%q", c.tier, c.target, got)
		}
	}
}

func TestPromptCacheKeyNotesPerTarget(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 16,
		PromptCacheKey: "secret-ish-client-chosen-key"}
	for _, proto := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses} {
		if got := lossyFor(t, proto, req); strings.Contains(got, "prompt_cache_key") {
			t.Errorf("%s 有槽位，不该报：%q", proto, got)
		}
	}
	for _, proto := range []string{codec.ProtocolAnthropic, codec.ProtocolGemini} {
		got := lossyFor(t, proto, req)
		if !strings.Contains(got, "dropped prompt_cache_key") {
			t.Errorf("%s 无缓存路由键，应报丢弃：%q", proto, got)
		}
		if strings.Contains(got, "secret-ish-client-chosen-key") {
			t.Errorf("%s 回显了客户端自选键值：%q", proto, got)
		}
	}
}

func TestRoutingParamsSilentWhenAbsent(t *testing.T) {
	for _, proto := range []string{codec.ProtocolAnthropic, codec.ProtocolChatCompletions,
		codec.ProtocolResponses, codec.ProtocolGemini} {
		got := lossyFor(t, proto, &ir.Request{Model: "m", MaxTokens: 16})
		if strings.Contains(got, "service_tier") || strings.Contains(got, "prompt_cache_key") {
			t.Errorf("%s 两维缺省误报：%q", proto, got)
		}
	}
}
