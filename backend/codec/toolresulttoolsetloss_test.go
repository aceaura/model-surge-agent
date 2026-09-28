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

// 轮次44：anthropic tool_result 块的 toolset_name（beta toolsets 归属名，配对
// tool_use 所属的 toolset 家族）是纯 provenance，只有 anthropic 有槽位。同族逐字
// 往返无损；投给外族（chat/responses/gemini 的工具结果形状都没有 toolset 槽位）
// 整维丢弃，此前完全静默——与 tool_use 的 caller/toolset_name（轮次28）同款。
// 这组测试钉住跨族请求侧有损诊断报出、anthropic 自家静默、缺席静默。

func toolsetToolResultBlock() ir.Block {
	return ir.Block{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
		ToolUseID:   "toolu_1",
		Content:     []ir.Block{{Type: ir.BlockText, Text: "ok"}},
		ToolsetName: "ts_beta",
	}}
}

func TestDiagnoseToolResultToolsetDroppedOffAnthropic(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{toolsetToolResultBlock()}},
	}}
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolGemini} {
		oc, _ := codec.Outbound(name)
		got := strings.Join(codec.DescribeLossy(req, name, oc.Caps()), "; ")
		if !strings.Contains(got, "tool result carries an anthropic-only toolset_name") {
			t.Errorf("%s 应报 tool_result toolset_name 丢失：%q", name, got)
		}
	}
	oc, _ := codec.Outbound(codec.ProtocolAnthropic)
	if notes := codec.DescribeLossy(req, codec.ProtocolAnthropic, oc.Caps()); len(notes) != 0 {
		t.Errorf("anthropic 自家接得住，误报：%v", notes)
	}
}

// 缺席不报：没有 toolset_name 的普通 tool_result 跨族不该凭空多出注记。
func TestNoToolResultToolsetNoteWhenAbsent(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
			ToolUseID: "toolu_1",
			Content:   []ir.Block{{Type: ir.BlockText, Text: "ok"}},
		}}}},
	}}
	oc, _ := codec.Outbound(codec.ProtocolChatCompletions)
	for _, n := range codec.DescribeLossy(req, codec.ProtocolChatCompletions, oc.Caps()) {
		if strings.Contains(n, "tool result carries an anthropic-only toolset_name") {
			t.Errorf("缺席 toolset_name 却报了注记：%q", n)
		}
	}
}
