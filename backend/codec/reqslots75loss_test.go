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

// #75 三字段的跨族可见性：同族静默（槽位原样送达），异族必须报出。
// lossyFor 与 boolPtr/intPtr 复用同包既有助手。

func TestMaxToolCallsNotesPerTarget(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 16, MaxToolCalls: intPtr(3)}
	if got := lossyFor(t, codec.ProtocolResponses, req); strings.Contains(got, "max_tool_calls") {
		t.Errorf("responses 有槽位，不该报：%q", got)
	}
	for _, proto := range []string{codec.ProtocolChatCompletions, codec.ProtocolAnthropic, codec.ProtocolGemini} {
		got := lossyFor(t, proto, req)
		if !strings.Contains(got, "dropped max_tool_calls") {
			t.Errorf("%s 无计数闸门，应报丢弃：%q", proto, got)
		}
	}
}

func TestIncludeObfuscationNotesPerTarget(t *testing.T) {
	// 显式 false 最要紧：客户端要关掉上游默认的混淆保护，跨族传达不了
	// 就必须让它知道。
	req := &ir.Request{Model: "m", MaxTokens: 16, IncludeObfuscation: boolPtr(false)}
	for _, proto := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses} {
		if got := lossyFor(t, proto, req); strings.Contains(got, "include_obfuscation") {
			t.Errorf("%s 有槽位，不该报：%q", proto, got)
		}
	}
	for _, proto := range []string{codec.ProtocolAnthropic, codec.ProtocolGemini} {
		got := lossyFor(t, proto, req)
		if !strings.Contains(got, "dropped stream_options.include_obfuscation") {
			t.Errorf("%s 无混淆机制，应报丢弃：%q", proto, got)
		}
	}
}

func TestResponseFormatDescriptionNotesPerTarget(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 16,
		ResponseFormat: &ir.ResponseFormat{
			Kind:        ir.ResponseFormatSchema,
			Name:        "n",
			Schema:      `{"type":"object"}`,
			Description: "输出一个点",
		}}
	for _, proto := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses} {
		if got := lossyFor(t, proto, req); strings.Contains(got, "description") {
			t.Errorf("%s 有 description 槽位，不该报：%q", proto, got)
		}
	}
	for _, proto := range []string{codec.ProtocolAnthropic, codec.ProtocolGemini} {
		got := lossyFor(t, proto, req)
		if !strings.Contains(got, "dropped response_format.description") {
			t.Errorf("%s 的 schema 槽位无 description 键，应报丢弃：%q", proto, got)
		}
	}
	// 没写 description 时不报：缺省无需传达。
	plain := &ir.Request{Model: "m", MaxTokens: 16,
		ResponseFormat: &ir.ResponseFormat{
			Kind: ir.ResponseFormatSchema, Name: "n", Schema: `{"type":"object"}`,
		}}
	if got := lossyFor(t, codec.ProtocolAnthropic, plain); strings.Contains(got, "description") {
		t.Errorf("没给 description 却报了：%q", got)
	}
}
