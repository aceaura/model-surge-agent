package chatcompletions

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 废弃 functions/function_call 折进现代槽位：功能等价无需注记；
// 现代键同给时现代键胜出；指名形态 {"name":"x"} 走扁平回落。
func TestDeprecatedFunctionsFoldIn(t *testing.T) {
	req, err := DecodeRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"functions":[{"name":"legacy_fn","description":"d","parameters":{"type":"object"}}],` +
		`"function_call":{"name":"legacy_fn"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "legacy_fn" ||
		req.Tools[0].Description != "d" || !strings.Contains(req.Tools[0].Schema, `"type":"object"`) {
		t.Fatalf("functions 没折进 Tools：%+v", req.Tools)
	}
	if req.ToolChoice == nil || req.ToolChoice.Mode != ir.ToolChoiceTool || req.ToolChoice.Name != "legacy_fn" {
		t.Fatalf("function_call 指名没折进 ToolChoice：%+v", req.ToolChoice)
	}
	// 编码只产出现代键，不回吐废弃键。
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, `"functions"`) || strings.Contains(s, `"function_call"`) {
		t.Fatalf("编码吐出了废弃键：%s", s)
	}
	if !strings.Contains(s, `"tools"`) || !strings.Contains(s, `"tool_choice"`) {
		t.Fatalf("现代键缺失：%s", s)
	}

	// function_call 的字符串形态（"none"/"auto"）同值集生效。
	req2, err := DecodeRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"functions":[{"name":"f"}],"function_call":"none"}`))
	if err != nil {
		t.Fatal(err)
	}
	if req2.ToolChoice == nil || req2.ToolChoice.Mode != ir.ToolChoiceNone {
		t.Fatalf(`function_call:"none" 未生效：%+v`, req2.ToolChoice)
	}

	// 现代键同给时现代键胜出（tools 取代 functions，tool_choice 取代 function_call）。
	req3, err := DecodeRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"functions":[{"name":"legacy_fn"}],"function_call":"none",` +
		`"tools":[{"type":"function","function":{"name":"modern_fn"}}],"tool_choice":"auto"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(req3.Tools) != 2 || req3.Tools[0].Name != "modern_fn" {
		t.Fatalf("现代 tools 应在前：%+v", req3.Tools)
	}
	if req3.ToolChoice == nil || req3.ToolChoice.Mode != ir.ToolChoiceAuto {
		t.Fatalf("现代 tool_choice 应胜出：%+v", req3.ToolChoice)
	}
}

// 带 type 而无 function.name 的 tool_choice 仍按畸形拒绝：那是现代键写坏了，
// 不是废弃 function_call 的扁平形态，扁平回落不能把它放进来。
func TestToolChoiceFlatFallbackRejectsTypedNameless(t *testing.T) {
	if _, err := DecodeRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"function"}}`)); err == nil {
		t.Fatal("带 type 无名字应 400")
	}
	req, err := DecodeRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"function_call":{"name":"flat_fn"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.ToolChoice == nil || req.ToolChoice.Name != "flat_fn" {
		t.Fatalf("扁平指名未命中：%+v", req.ToolChoice)
	}
}

// responses 的消息数上限档外族归 length：输出确实不完整，
// 客户端不能把半截结果当终稿（stop 会）。
func TestMaxMessagesRendersLength(t *testing.T) {
	if got := renderFinishReason(ir.StopMaxMessages); got != "length" {
		t.Fatalf("renderFinishReason(StopMaxMessages) = %q, want length", got)
	}
}
