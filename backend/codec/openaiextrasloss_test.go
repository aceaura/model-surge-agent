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

// #24 跨族可见性：verbosity 只有 OpenAI 两系有槽位；safety_identifier
// 三家可达（anthropic 借 metadata.user_id）但槽位冲突要报；moderation 与
// prompt_cache_options 是 OpenAI 两系专属。标识值不回显。
// lossyFor 复用同包既有助手。

func TestVerbosityNotesPerTarget(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 16, Verbosity: "low"}
	for _, proto := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses} {
		if got := lossyFor(t, proto, req); strings.Contains(got, "verbosity") {
			t.Errorf("%s 有槽位，不该报：%q", proto, got)
		}
	}
	for _, proto := range []string{codec.ProtocolAnthropic, codec.ProtocolGemini} {
		if got := lossyFor(t, proto, req); !strings.Contains(got, "dropped verbosity") {
			t.Errorf("%s 无输出长度转向，应报丢弃：%q", proto, got)
		}
	}
}

func TestSafetyIdentifierNotesPerTarget(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 16, SafetyIdentifier: "si-secret"}
	// OpenAI 两系原生槽位、anthropic 槽空着可映：静默且不回显值。
	for _, proto := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolAnthropic} {
		got := lossyFor(t, proto, req)
		if strings.Contains(got, "safety_identifier") {
			t.Errorf("%s 可达，不该报：%q", proto, got)
		}
		if strings.Contains(got, "si-secret") {
			t.Errorf("%s 回显了标识值：%q", proto, got)
		}
	}
	// anthropic 槽被 user_id 占了：挤不进去，报出。
	busy := &ir.Request{Model: "m", MaxTokens: 16, SafetyIdentifier: "si-secret",
		Metadata: map[string]string{"user_id": "u1"}}
	got := lossyFor(t, codec.ProtocolAnthropic, busy)
	if !strings.Contains(got, "dropped safety_identifier") || !strings.Contains(got, "already carries a user id") {
		t.Errorf("anthropic 槽位冲突未报：%q", got)
	}
	if strings.Contains(got, "si-secret") {
		t.Errorf("anthropic 回显了标识值：%q", got)
	}
	// gemini 无任何用户标识槽位。
	got = lossyFor(t, codec.ProtocolGemini, req)
	if !strings.Contains(got, "dropped safety_identifier") {
		t.Errorf("gemini 应报丢弃：%q", got)
	}
}

func TestModerationAndCacheOptionsNotesPerTarget(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 16,
		Moderation:         []byte(`{"model":"mod"}`),
		PromptCacheOptions: []byte(`{"mode":"auto"}`)}
	for _, proto := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses} {
		got := lossyFor(t, proto, req)
		if strings.Contains(got, "moderation") || strings.Contains(got, "prompt_cache_options") {
			t.Errorf("%s 有槽位，不该报：%q", proto, got)
		}
	}
	for _, proto := range []string{codec.ProtocolAnthropic, codec.ProtocolGemini} {
		got := lossyFor(t, proto, req)
		if !strings.Contains(got, "dropped moderation") {
			t.Errorf("%s 无审核策略槽位，应报丢弃：%q", proto, got)
		}
		if !strings.Contains(got, "dropped prompt_cache_options") {
			t.Errorf("%s 无显式缓存断点，应报丢弃：%q", proto, got)
		}
	}
}
