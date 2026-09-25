package codec_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	_ "github.com/aceaura/model-surge-agent/backend/codec/anthropic"
	_ "github.com/aceaura/model-surge-agent/backend/codec/chatcompletions"
	_ "github.com/aceaura/model-surge-agent/backend/codec/gemini"
	_ "github.com/aceaura/model-surge-agent/backend/codec/responses"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// anthropic 工具修饰四维（defer_loading / eager_input_streaming /
// input_examples / allowed_callers）跨族零泄漏与诊断：四维任一出现即计数
// 该工具；anthropic 自家静默；全缺省四家静默。
//
// 对应旧仓 #27（a9d0095）。

func modifiersRequest() *ir.Request {
	fa := false
	return &ir.Request{
		Model: "m", MaxTokens: 100,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		Tools: []ir.Tool{{
			Name: "ping", Schema: `{"type":"object"}`,
			DeferLoading: true, EagerInputStreaming: &fa,
			InputExamples:  []json.RawMessage{[]byte(`{"name":"x"}`)},
			AllowedCallers: []string{"direct"},
		}},
	}
}

func TestToolModifiersNeverLeakToOtherFamilies(t *testing.T) {
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolGemini} {
		oc, ok := codec.Outbound(name)
		if !ok {
			t.Fatalf("outbound %q not registered", name)
		}
		out, err := oc.EncodeRequest(modifiersRequest())
		if err != nil {
			t.Fatalf("%s EncodeRequest: %v", name, err)
		}
		body := string(out)
		for _, probe := range []string{"defer_loading", "eager_input_streaming", "input_examples", "allowed_callers"} {
			if strings.Contains(body, probe) {
				t.Errorf("%s 泄漏 %q: %s", name, probe, body)
			}
		}
	}
}

func TestDiagnoseToolModifiersDroppedOffAnthropic(t *testing.T) {
	fa := false
	req := &ir.Request{
		Model: "m", MaxTokens: 10, Tools: []ir.Tool{
			{Name: "a", DeferLoading: true},
			{Name: "b", EagerInputStreaming: &fa},
			{Name: "c", InputExamples: []json.RawMessage{[]byte(`{}`)}},
			{Name: "d", AllowedCallers: []string{"direct"}},
			{Name: "e"}, // 无修饰不计
		},
	}
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolGemini} {
		oc, _ := codec.Outbound(name)
		got := strings.Join(codec.DescribeLossy(req, name, oc.Caps()), "; ")
		if !strings.Contains(got, "tool modifiers on 4 tool(s)") {
			t.Errorf("%s 应报 4 件工具的修饰丢失：%q", name, got)
		}
	}
	oc, _ := codec.Outbound(codec.ProtocolAnthropic)
	if notes := codec.DescribeLossy(req, codec.ProtocolAnthropic, oc.Caps()); len(notes) != 0 {
		t.Errorf("anthropic 自家接得住，误报：%v", notes)
	}
}

func TestDiagnoseToolModifiersSilentWhenAbsent(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10, Tools: []ir.Tool{{Name: "plain"}}}
	for _, name := range codec.OutboundNames() {
		oc, _ := codec.Outbound(name)
		if notes := codec.DescribeLossy(req, name, oc.Caps()); len(notes) != 0 {
			t.Errorf("%s 无修饰误报：%v", name, notes)
		}
	}
}
