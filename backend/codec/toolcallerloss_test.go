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

// 轮次28：anthropic tool_use / server_tool_use 块的 caller（发起方标记，官方
// 响应侧必填、请求侧可选的 union）与 toolset_name（beta toolsets 归属名）是纯
// provenance，只有 anthropic 有槽位。同族逐字往返无损；投给外族整维丢弃，此前
// 完全静默。这组测试钉住跨族请求侧有损诊断报出、anthropic 自家静默、缺席静默。

func callerToolUseBlock() ir.Block {
	return ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
		ID: "toolu_1", Name: "get_weather", Input: `{"city":"SF"}`,
		Caller:      json.RawMessage(`{"tool_id":"srvtoolu_9","type":"code_execution_20250825"}`),
		ToolsetName: "ts_beta",
	}}
}

func TestDiagnoseToolCallerDroppedOffAnthropic(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10, Messages: []ir.Message{
		{Role: ir.RoleAssistant, Content: []ir.Block{callerToolUseBlock()}},
	}}
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolGemini} {
		oc, _ := codec.Outbound(name)
		got := strings.Join(codec.DescribeLossy(req, name, oc.Caps()), "; ")
		if !strings.Contains(got, "caller/toolset_name") {
			t.Errorf("%s 应报 tool_use caller/toolset_name 丢失：%q", name, got)
		}
	}
	oc, _ := codec.Outbound(codec.ProtocolAnthropic)
	if notes := codec.DescribeLossy(req, codec.ProtocolAnthropic, oc.Caps()); len(notes) != 0 {
		t.Errorf("anthropic 自家接得住，误报：%v", notes)
	}
}

// 轮次74：server_tool_use 跨族被外族编码器整块跳过，caller 发起方标记随整块一起
// 消失，已由 ServerToolDropNote 统一报出——不再单设 caller 专项注记（那会与整块
// 注记重复计报同一次丢弃，也与 web_search_tool_result.caller 的处置不对称）。
// 本用例钉住：整块丢弃照常报、caller 不再单报、anthropic 自家静默。
func TestDiagnoseServerToolCallerDroppedOffAnthropic(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10, Messages: []ir.Message{
		{Role: ir.RoleAssistant, Content: []ir.Block{{
			Type:          ir.BlockServerToolUse,
			ServerToolUse: &ir.ServerToolUse{ID: "srvtoolu_1", Name: "web_search", Input: `{"q":"x"}`, Caller: json.RawMessage(`{"type":"direct"}`)},
		}}},
	}}
	oc, _ := codec.Outbound(codec.ProtocolChatCompletions)
	notes := codec.DescribeLossy(req, codec.ProtocolChatCompletions, oc.Caps())
	got := strings.Join(notes, "; ")
	// 整块丢弃照常报出，caller 随整块消失已含在其中。
	if !strings.Contains(got, "server-side tool call") {
		t.Errorf("应报 server_tool_use 整块丢弃：%q", got)
	}
	// 不再单设 caller 专项注记：那条会与整块注记叠报同一次丢弃（规则 a）。
	if strings.Contains(got, "caller/toolset_name") || strings.Contains(got, "server tool call caller") {
		t.Errorf("server_tool_use 的 caller 已被整块注记覆盖，不应再单报：%q", got)
	}
	if len(notes) != 1 {
		t.Errorf("server_tool_use 跨族应恰有一条注记，实得 %d：%v", len(notes), notes)
	}
	aoc, _ := codec.Outbound(codec.ProtocolAnthropic)
	if anotes := codec.DescribeLossy(req, codec.ProtocolAnthropic, aoc.Caps()); len(anotes) != 0 {
		t.Errorf("anthropic 自家接得住，误报：%v", anotes)
	}
}

// 只带 toolset_name（无 caller）也要报，反之亦然：两维任一非空即触发。
func TestDiagnoseToolsetOnlyStillNoted(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10, Messages: []ir.Message{
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
			ID: "toolu_1", Name: "f", Input: `{}`, ToolsetName: "ts_beta",
		}}}},
	}}
	oc, _ := codec.Outbound(codec.ProtocolResponses)
	got := strings.Join(codec.DescribeLossy(req, codec.ProtocolResponses, oc.Caps()), "; ")
	if !strings.Contains(got, "caller/toolset_name") {
		t.Errorf("只带 toolset_name 也应报：%q", got)
	}
}

// 缺席不报：没有 caller / toolset_name 的普通 tool_use 跨族不该凭空多出注记。
func TestNoCallerNoteWhenAbsent(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10, Messages: []ir.Message{
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
			ID: "toolu_1", Name: "f", Input: `{}`,
		}}}},
	}}
	oc, _ := codec.Outbound(codec.ProtocolChatCompletions)
	for _, n := range codec.DescribeLossy(req, codec.ProtocolChatCompletions, oc.Caps()) {
		if strings.Contains(n, "caller/toolset_name") {
			t.Errorf("缺席 caller/toolset_name 却报了注记：%q", n)
		}
	}
}
