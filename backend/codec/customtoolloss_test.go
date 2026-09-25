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

// responses 的 custom_tool_call（自由文本入参）跨族降级：三个外族只有
// JSON 参数槽位，调用以 {"input":…} 投影落进去——内容不丢、形态降级，
// 且投影恒为合法对象，不得触发「畸形参数」误报；降级由 DescribeLossy
// 报出。responses 同族原样往返，报了就是谎报。

func customToolRequest() *ir.Request {
	return &ir.Request{
		Model: "m", MaxTokens: 16,
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
			{Role: ir.RoleAssistant, Content: []ir.Block{{
				Type: ir.BlockToolUse,
				ToolUse: &ir.ToolUse{
					ID: "c1", Name: "grep", Kind: ir.ToolCustom,
					InputText: "自由文本", Input: `{"input":"自由文本"}`,
				},
			}}},
			{Role: ir.RoleUser, Content: []ir.Block{{
				Type: ir.BlockToolResult,
				ToolResult: &ir.ToolResult{
					ToolUseID: "c1", Kind: ir.ToolCustom,
					Content: []ir.Block{{Type: ir.BlockText, Text: "ok"}},
				},
			}}},
		},
	}
}

func TestCustomToolCrossFamilyProjection(t *testing.T) {
	req := customToolRequest()
	for _, name := range codec.OutboundNames() {
		oc, _ := codec.Outbound(name)
		body, err := oc.EncodeRequest(req)
		if err != nil {
			t.Fatalf("%s: EncodeRequest err=%v", name, err)
		}
		s := string(body)
		switch name {
		case codec.ProtocolResponses:
			// 同族：自由文本原文回 custom_tool_call 条目，不得出现投影。
			if !strings.Contains(s, `"type":"custom_tool_call"`) ||
				!strings.Contains(s, `"input":"自由文本"`) {
				t.Errorf("responses 同族没原样往返: %s", s)
			}
			if strings.Contains(s, `\"input\":`) {
				t.Errorf("responses 同族写回了投影: %s", s)
			}
			if strings.Contains(s, `"type":"custom_tool_call_output"`) == false {
				t.Errorf("responses 同族结果条目类型不对: %s", s)
			}
		case codec.ProtocolAnthropic:
			// input 是对象槽位，投影原样落进去（不转义）。
			if !strings.Contains(s, `"input":{"input":"自由文本"}`) {
				t.Errorf("anthropic 没落投影: %s", s)
			}
		case codec.ProtocolGemini:
			if !strings.Contains(s, `{"input":"自由文本"}`) {
				t.Errorf("gemini 没落投影: %s", s)
			}
		default: // chat_completions：arguments 是字符串槽位，投影被转义嵌入。
			if !strings.Contains(s, `\"input\":\"自由文本\"`) {
				t.Errorf("chat 没落投影: %s", s)
			}
		}
	}
}

func TestCustomToolDowngradeNotes(t *testing.T) {
	req := customToolRequest()
	for _, name := range codec.OutboundNames() {
		oc, _ := codec.Outbound(name)
		notes := strings.Join(codec.DescribeLossy(req, name, oc.Caps()), "; ")
		if name == codec.ProtocolResponses {
			if strings.Contains(notes, "custom tool") {
				t.Errorf("responses 同族谎报降级: %q", notes)
			}
			continue
		}
		if !strings.Contains(notes, "downgraded 1 custom tool call(s) to function calls") {
			t.Errorf("%s 调用降级未报: %q", name, notes)
		}
		if !strings.Contains(notes, "downgraded 1 custom tool output(s) to ordinary function results") {
			t.Errorf("%s 结果降级未报: %q", name, notes)
		}
		// 投影恒为合法 JSON 对象：不得与「畸形参数」混报。
		if strings.Contains(notes, "malformed") {
			t.Errorf("%s 把投影误报成畸形参数: %q", name, notes)
		}
	}
}

// function 形态的调用与结果不受影响：没有 custom 注记，参数原样。
func TestFunctionToolNoCustomNotes(t *testing.T) {
	req := &ir.Request{
		Model: "m", MaxTokens: 16,
		Messages: []ir.Message{
			{Role: ir.RoleAssistant, Content: []ir.Block{{
				Type:    ir.BlockToolUse,
				ToolUse: &ir.ToolUse{ID: "c1", Name: "f", Input: `{"a":1}`},
			}}},
			{Role: ir.RoleUser, Content: []ir.Block{{
				Type: ir.BlockToolResult,
				ToolResult: &ir.ToolResult{ToolUseID: "c1",
					Content: []ir.Block{{Type: ir.BlockText, Text: "ok"}}},
			}}},
		},
	}
	for _, name := range codec.OutboundNames() {
		oc, _ := codec.Outbound(name)
		if notes := codec.DescribeLossy(req, name, oc.Caps()); len(notes) != 0 {
			t.Errorf("%s 对普通函数调用误报: %v", name, notes)
		}
	}
}
