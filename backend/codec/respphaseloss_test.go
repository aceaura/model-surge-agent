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

// 轮次38：responses assistant message 的 phase（commentary|final_answer）是 responses
// 一族独有维度，官方要求 preserve-and-resend。同族逐字往返无损；投给外族整维丢弃，此前
// 完全静默。这组测试钉住跨族请求侧有损诊断报出、responses 自家静默、缺席静默。

func phaseReq() *ir.Request {
	return &ir.Request{Model: "m", MaxTokens: 10, Messages: []ir.Message{
		{Role: ir.RoleAssistant, ResponsesPhase: "final_answer",
			Content: []ir.Block{{Type: ir.BlockText, Text: "ans"}}},
	}}
}

func TestDiagnoseRespPhaseDroppedOffResponses(t *testing.T) {
	req := phaseReq()
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolAnthropic, codec.ProtocolGemini} {
		oc, _ := codec.Outbound(name)
		got := strings.Join(codec.DescribeLossy(req, name, oc.Caps()), "; ")
		if !strings.Contains(got, "message phase") {
			t.Errorf("%s 应报 responses message phase 丢失：%q", name, got)
		}
	}
	oc, _ := codec.Outbound(codec.ProtocolResponses)
	if notes := codec.DescribeLossy(req, codec.ProtocolResponses, oc.Caps()); len(notes) != 0 {
		t.Errorf("responses 自家接得住，误报：%v", notes)
	}
}

// 缺席不报：没有 phase 的普通助手消息跨族不该凭空多出注记。
func TestNoRespPhaseNoteWhenAbsent(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10, Messages: []ir.Message{
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: "x"}}},
	}}
	oc, _ := codec.Outbound(codec.ProtocolChatCompletions)
	for _, n := range codec.DescribeLossy(req, codec.ProtocolChatCompletions, oc.Caps()) {
		if strings.Contains(n, "message phase") {
			t.Errorf("缺席 phase 却报了注记：%q", n)
		}
	}
}
