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

// response_format.strict 与 .name 只有 OpenAI 两系（chat / responses）有槽位；
// anthropic 的 output_config.format 与 gemini 的 responseSchema 都没有这两键。
// 跨族到 anthropic/gemini 时，客户端显式要的 strict:false（放宽 schema）与
// name（标识/缓存键）此前完全静默丢失。

func describeRF(t *testing.T, target string, rf *ir.ResponseFormat) string {
	t.Helper()
	oc, ok := codec.Outbound(target)
	if !ok {
		t.Fatalf("outbound %q not registered", target)
	}
	req := &ir.Request{Model: "m", MaxTokens: 10, ResponseFormat: rf,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
	return strings.Join(codec.DescribeLossy(req, target, oc.Caps()), "; ")
}

// 显式 strict:false 投到无开关的协议要报出：客户端的放宽到不了目标，
// 输出反被恒严格语义过度约束。
func TestResponseFormatNonStrictLossReported(t *testing.T) {
	rf := &ir.ResponseFormat{Kind: ir.ResponseFormatSchema,
		Schema: `{"type":"object","properties":{"a":{"type":"string"}}}`, Strict: boolPtr(false)}
	for _, target := range []string{codec.ProtocolAnthropic, codec.ProtocolGemini} {
		got := describeRF(t, target, rf)
		if !strings.Contains(got, "response_format.strict") {
			t.Errorf("%s 丢弃 strict:false 未报告：%q", target, got)
		}
	}
	// OpenAI 两系有 strict 槽位，不得误报。
	for _, target := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses} {
		got := describeRF(t, target, rf)
		if strings.Contains(got, "response_format.strict") {
			t.Errorf("%s 接得住 strict，误报：%q", target, got)
		}
	}
}

// 显式 strict:true 与目标的恒严格语义同义，不算丢失，不报。
func TestResponseFormatStrictTrueNotReported(t *testing.T) {
	rf := &ir.ResponseFormat{Kind: ir.ResponseFormatSchema,
		Schema: `{"type":"object","properties":{"a":{"type":"string"}}}`, Strict: boolPtr(true)}
	for _, target := range []string{codec.ProtocolAnthropic, codec.ProtocolGemini} {
		got := describeRF(t, target, rf)
		if strings.Contains(got, "response_format.strict") {
			t.Errorf("%s strict:true 被误报为丢失：%q", target, got)
		}
	}
}

// name 投到无 name 键的协议要报出。
func TestResponseFormatNameLossReported(t *testing.T) {
	rf := &ir.ResponseFormat{Kind: ir.ResponseFormatSchema, Name: "my_schema",
		Schema: `{"type":"object","properties":{"a":{"type":"string"}}}`}
	for _, target := range []string{codec.ProtocolAnthropic, codec.ProtocolGemini} {
		got := describeRF(t, target, rf)
		if !strings.Contains(got, "response_format.name") {
			t.Errorf("%s 丢弃 name 未报告：%q", target, got)
		}
	}
	for _, target := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses} {
		got := describeRF(t, target, rf)
		if strings.Contains(got, "response_format.name") {
			t.Errorf("%s 接得住 name，误报：%q", target, got)
		}
	}
}

// 没给 strict（nil）也没给 name 时不得报这两条。
func TestResponseFormatNoStrictNoNameSilent(t *testing.T) {
	rf := &ir.ResponseFormat{Kind: ir.ResponseFormatSchema,
		Schema: `{"type":"object","properties":{"a":{"type":"string"}}}`}
	got := describeRF(t, codec.ProtocolGemini, rf)
	if strings.Contains(got, "response_format.strict") || strings.Contains(got, "response_format.name") {
		t.Errorf("未表态却报了 strict/name 丢失：%q", got)
	}
}
