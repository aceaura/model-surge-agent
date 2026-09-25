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

// responses 的 typed tool_choice（mcp/file_search 等无 name 变体，IR 的
// Raw 不透明槽）跨族出站整条编不出：外族的 tool_choice 形状只有
// auto/any/none/具名函数四档。丢弃必须由 DescribeLossy 报出；responses
// 同族原样回写，报了就是谎报。
func typedChoiceRequest() *ir.Request {
	return &ir.Request{
		Model: "m", MaxTokens: 16,
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
		},
		Tools: []ir.Tool{{Name: "alpha", Schema: `{"type":"object","properties":{}}`}},
		ToolChoice: &ir.ToolChoice{
			Raw: json.RawMessage(`{"type":"mcp","server_label":"dmcp"}`),
		},
	}
}

func TestTypedToolChoiceCrossFamilyNotes(t *testing.T) {
	req := typedChoiceRequest()
	for _, name := range codec.OutboundNames() {
		oc, _ := codec.Outbound(name)
		notes := strings.Join(codec.DescribeLossy(req, name, oc.Caps()), "; ")
		if name == codec.ProtocolResponses {
			if strings.Contains(notes, "tool_choice") {
				t.Errorf("responses 同族谎报 typed tool_choice 降级: %q", notes)
			}
			body, err := oc.EncodeRequest(req)
			if err != nil {
				t.Fatalf("responses EncodeRequest: %v", err)
			}
			if !strings.Contains(string(body), `"tool_choice":{"type":"mcp","server_label":"dmcp"}`) {
				t.Errorf("responses 同族没原样回写: %s", body)
			}
			continue
		}
		if !strings.Contains(notes, "typed tool-choice variant") {
			t.Errorf("%s 没报 typed tool_choice 降级: %q", name, notes)
		}
		body, err := oc.EncodeRequest(req)
		if err != nil {
			t.Fatalf("%s EncodeRequest: %v", name, err)
		}
		if strings.Contains(string(body), "dmcp") {
			t.Errorf("%s 把编不出的 typed tool_choice 漏进了请求体: %s", name, body)
		}
	}
}

// 带 Mode 的指名变体不在 typed 注记范围内：结构化字段各族照常编码。
func TestNamedToolChoiceNoTypedNote(t *testing.T) {
	req := typedChoiceRequest()
	req.ToolChoice = &ir.ToolChoice{Mode: ir.ToolChoiceTool, Name: "alpha",
		Raw: json.RawMessage(`{"type":"function","name":"alpha"}`)}
	for _, name := range codec.OutboundNames() {
		oc, _ := codec.Outbound(name)
		notes := strings.Join(codec.DescribeLossy(req, name, oc.Caps()), "; ")
		if strings.Contains(notes, "typed tool-choice variant") {
			t.Errorf("%s 对已建模指名变体误报 typed 降级: %q", name, notes)
		}
	}
}
