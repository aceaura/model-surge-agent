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

// prompt_cache_retention（OpenAI 两系缓存最大留存策略 in_memory|24h，关乎 ZDR
// 合规）此前 chat/responses 两个解码器都用裸 json.Unmarshal 把它静默吞掉，而它的
// 两个兄弟 prompt_cache_key / prompt_cache_options 都既建模又报出。本轮补齐：
// 同族与 OpenAI 两系互转原样承载，跨族到 anthropic/gemini 丢弃并报出，null/缺席归一。
//
// 对应轮次23（wire 字段覆盖系统性广审）。

const (
	chatRetentionBody = `{"model":"m","messages":[{"role":"user","content":"hi"}],"prompt_cache_retention":"24h"}`
	respRetentionBody = `{"model":"m","input":"hi","prompt_cache_retention":"in_memory"}`
)

// 同族往返：chat→chat、responses→responses 都要原样承载值。
func TestCacheRetentionSameFamilyCarry(t *testing.T) {
	req := decodeReq(t, codec.ProtocolChatCompletions, chatRetentionBody)
	if req.PromptCacheRetention != "24h" {
		t.Fatalf("chat 解码没收下 prompt_cache_retention：%q", req.PromptCacheRetention)
	}
	if out := string(encodeOut(t, codec.ProtocolChatCompletions, req)); !strings.Contains(out, `"prompt_cache_retention":"24h"`) {
		t.Errorf("chat 同族回写丢了 prompt_cache_retention：%s", out)
	}

	req = decodeReq(t, codec.ProtocolResponses, respRetentionBody)
	if req.PromptCacheRetention != "in_memory" {
		t.Fatalf("responses 解码没收下 prompt_cache_retention：%q", req.PromptCacheRetention)
	}
	if out := string(encodeOut(t, codec.ProtocolResponses, req)); !strings.Contains(out, `"prompt_cache_retention":"in_memory"`) {
		t.Errorf("responses 同族回写丢了 prompt_cache_retention：%s", out)
	}
}

// OpenAI 两系互转：chat 与 responses 都有槽位，值必须承载、且不报丢弃。
func TestCacheRetentionCrossOpenAICarry(t *testing.T) {
	// chat → responses
	req := decodeReq(t, codec.ProtocolChatCompletions, chatRetentionBody)
	if out := string(encodeOut(t, codec.ProtocolResponses, req)); !strings.Contains(out, `"prompt_cache_retention":"24h"`) {
		t.Errorf("chat→responses 丢了 prompt_cache_retention：%s", out)
	}
	if got := lossyFor(t, codec.ProtocolResponses, req); strings.Contains(got, "prompt_cache_retention") {
		t.Errorf("responses 有槽位，不该报丢弃：%q", got)
	}
	// responses → chat
	req = decodeReq(t, codec.ProtocolResponses, respRetentionBody)
	if out := string(encodeOut(t, codec.ProtocolChatCompletions, req)); !strings.Contains(out, `"prompt_cache_retention":"in_memory"`) {
		t.Errorf("responses→chat 丢了 prompt_cache_retention：%s", out)
	}
	if got := lossyFor(t, codec.ProtocolChatCompletions, req); strings.Contains(got, "prompt_cache_retention") {
		t.Errorf("chat 有槽位，不该报丢弃：%q", got)
	}
}

// 跨族到 anthropic/gemini：无槽位，丢弃并报出（措辞点名 ZDR 合规语义）。
func TestCacheRetentionDroppedOffOpenAI(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 16, PromptCacheRetention: "in_memory",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
	for _, proto := range []string{codec.ProtocolAnthropic, codec.ProtocolGemini} {
		got := lossyFor(t, proto, req)
		if !strings.Contains(got, "dropped prompt_cache_retention") {
			t.Errorf("%s 无缓存留存槽位，应报丢弃：%q", proto, got)
		}
		if !strings.Contains(got, "zero-data-retention") {
			t.Errorf("%s 注记应点名 ZDR 合规语义：%q", proto, got)
		}
		// 出站线格式不得泄漏该键。
		if out := string(encodeOut(t, proto, req)); strings.Contains(out, "prompt_cache_retention") {
			t.Errorf("%s 出站泄漏 prompt_cache_retention：%s", proto, out)
		}
	}
	// OpenAI 两系有槽位，静默。
	for _, proto := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses} {
		if got := lossyFor(t, proto, req); strings.Contains(got, "prompt_cache_retention") {
			t.Errorf("%s 有槽位，不该报：%q", proto, got)
		}
	}
}

// 显式 null 与缺席都归一为空，且缺席时不凭空造键。
func TestCacheRetentionNullAndAbsentNormalized(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"prompt_cache_retention":null}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
	} {
		req := decodeReq(t, codec.ProtocolChatCompletions, body)
		if req.PromptCacheRetention != "" {
			t.Errorf("null/缺席应归一为空：%q", req.PromptCacheRetention)
		}
		if out := string(encodeOut(t, codec.ProtocolChatCompletions, req)); strings.Contains(out, "prompt_cache_retention") {
			t.Errorf("null/缺席不应出键：%s", out)
		}
		// 归一为空后跨族也不该报（没有可丢的东西）。
		if got := lossyFor(t, codec.ProtocolGemini, req); strings.Contains(got, "prompt_cache_retention") {
			t.Errorf("空值跨族误报：%q", got)
		}
	}
}
