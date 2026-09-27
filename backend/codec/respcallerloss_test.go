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

// 轮次37：responses function_call/custom_tool_call 条目的 caller/namespace/async
// 是 responses 一族独有的纯 provenance，只有 responses 有槽位。同族逐字往返无损；
// 投给外族整维丢弃，此前完全静默。这组测试钉住跨族请求侧有损诊断报出、responses
// 自家静默、缺席静默，以及与 anthropic caller 注记互不串味。

func respCallerBlock(async bool) ir.Block {
	return ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
		ID: "call_1", Name: "get", Input: `{}`,
		ResponsesCaller:    json.RawMessage(`{"type":"program"}`),
		ResponsesNamespace: "ns_a",
		ResponsesAsync:     &async,
	}}
}

func TestDiagnoseRespCallerDroppedOffResponses(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10, Messages: []ir.Message{
		{Role: ir.RoleAssistant, Content: []ir.Block{respCallerBlock(true)}},
	}}
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolAnthropic, codec.ProtocolGemini} {
		oc, _ := codec.Outbound(name)
		got := strings.Join(codec.DescribeLossy(req, name, oc.Caps()), "; ")
		if !strings.Contains(got, "caller/namespace/async") {
			t.Errorf("%s 应报 responses caller/namespace/async 丢失：%q", name, got)
		}
	}
	oc, _ := codec.Outbound(codec.ProtocolResponses)
	if notes := codec.DescribeLossy(req, codec.ProtocolResponses, oc.Caps()); len(notes) != 0 {
		t.Errorf("responses 自家接得住，误报：%v", notes)
	}
}

// 只带其中一维也要报：三维任一非空即触发。async:false 也算「带」（指针非 nil）。
func TestDiagnoseRespCallerAnyDimensionNoted(t *testing.T) {
	asyncFalse := false
	cases := map[string]ir.Block{
		"caller_only":    {Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "c", Name: "f", Input: `{}`, ResponsesCaller: json.RawMessage(`{"type":"program"}`)}},
		"namespace_only": {Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "c", Name: "f", Input: `{}`, ResponsesNamespace: "ns"}},
		"async_false":    {Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "c", Name: "f", Input: `{}`, ResponsesAsync: &asyncFalse}},
	}
	for label, blk := range cases {
		req := &ir.Request{Model: "m", MaxTokens: 10, Messages: []ir.Message{
			{Role: ir.RoleAssistant, Content: []ir.Block{blk}},
		}}
		oc, _ := codec.Outbound(codec.ProtocolChatCompletions)
		got := strings.Join(codec.DescribeLossy(req, codec.ProtocolChatCompletions, oc.Caps()), "; ")
		if !strings.Contains(got, "caller/namespace/async") {
			t.Errorf("%s 应报注记：%q", label, got)
		}
	}
}

// 缺席不报：普通 tool_use 跨族不该凭空多出 responses provenance 注记，
// 也不该把 anthropic caller 注记串到 responses 三维上。
func TestNoRespCallerNoteWhenAbsent(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10, Messages: []ir.Message{
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
			ID: "c", Name: "f", Input: `{}`,
		}}}},
	}}
	oc, _ := codec.Outbound(codec.ProtocolChatCompletions)
	for _, n := range codec.DescribeLossy(req, codec.ProtocolChatCompletions, oc.Caps()) {
		if strings.Contains(n, "caller/namespace/async") {
			t.Errorf("缺席 responses provenance 却报了注记：%q", n)
		}
	}
}

// anthropic caller 与 responses caller 是两族独立标记：带 anthropic caller 的块
// 投给 responses 只该报 anthropic 那条（caller/toolset_name），不该误报 responses
// 三维；反之亦然。
func TestRespCallerNoteDoesNotCrossContaminate(t *testing.T) {
	anthReq := &ir.Request{Model: "m", MaxTokens: 10, Messages: []ir.Message{
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
			ID: "toolu_1", Name: "f", Input: `{}`,
			Caller: json.RawMessage(`{"type":"direct"}`), ToolsetName: "ts",
		}}}},
	}}
	oc, _ := codec.Outbound(codec.ProtocolResponses)
	got := strings.Join(codec.DescribeLossy(anthReq, codec.ProtocolResponses, oc.Caps()), "; ")
	if !strings.Contains(got, "caller/toolset_name") {
		t.Errorf("anthropic caller 投 responses 应报 caller/toolset_name：%q", got)
	}
	if strings.Contains(got, "caller/namespace/async") {
		t.Errorf("anthropic caller 不该误报 responses 三维：%q", got)
	}
}
