package ir

import (
	"reflect"
	"testing"
)

// custom 工具调用（自由文本入参）的聚合与重放性质：
// 流式增量累积与 splitBlock 重放两条路必须得到同一个终态——
// InputText 是原文、Input 是 {"input":…} 投影。

func TestCustomToolUseAggregatesInputText(t *testing.T) {
	var a Aggregator
	a.Add(Event{Type: EvMessageStart, MessageID: "m1", Model: "m"})
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{
		Type: BlockToolUse, ToolUse: &ToolUse{ID: "c1", Name: "grep", Kind: ToolCustom}}})
	a.Add(Event{Type: EvToolInput, Index: 0, Text: "free "})
	a.Add(Event{Type: EvToolInput, Index: 0, Text: "text"})
	a.Add(Event{Type: EvBlockStop, Index: 0})
	a.Add(Event{Type: EvMessageDelta, StopReason: StopToolUse})
	a.Add(Event{Type: EvMessageStop})

	got := a.Response()
	u := got.Content[0].ToolUse
	if u.Kind != ToolCustom {
		t.Errorf("Kind = %q", u.Kind)
	}
	if u.InputText != "free text" {
		t.Errorf("InputText = %q, 增量应累积进原文槽", u.InputText)
	}
	if u.Input != `{"input":"free text"}` {
		t.Errorf("Input = %q, 终态应是投影", u.Input)
	}
}

// function 形态不受影响：增量照旧落 Input，不发明投影。
func TestFunctionToolUseUnchangedByCustomFields(t *testing.T) {
	var a Aggregator
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{
		Type: BlockToolUse, ToolUse: &ToolUse{ID: "c1", Name: "f"}}})
	a.Add(Event{Type: EvToolInput, Index: 0, Text: `{"a":1}`})
	a.Add(Event{Type: EvBlockStop, Index: 0})
	a.Add(Event{Type: EvMessageStop})

	u := a.Response().Content[0].ToolUse
	if u.InputText != "" {
		t.Errorf("function 形态被发明了 InputText: %q", u.InputText)
	}
	if u.Input != `{"a":1}` {
		t.Errorf("Input = %q", u.Input)
	}
}

// splitBlock 重放 custom 调用：增量通道走 InputText 原文（不是投影），
// 重放后聚合终态与原响应等价。
func TestCustomToolUseReplayRoundTrip(t *testing.T) {
	want := Response{ID: "m1", Model: "m", StopReason: StopToolUse, Content: []Block{
		{Type: BlockToolUse, ToolUse: &ToolUse{
			ID: "c1", Name: "grep", Kind: ToolCustom,
			InputText: "free text", Input: `{"input":"free text"}`,
		}},
	}}
	var deltas []Event
	var agg Aggregator
	for _, e := range ResponseEvents(&want) {
		if e.Type == EvToolInput {
			deltas = append(deltas, e)
		}
		agg.Add(e)
	}
	if len(deltas) != 1 || deltas[0].Text != "free text" {
		t.Fatalf("重放增量应只发原文一条: %+v", deltas)
	}
	got := agg.Response()
	if len(got.Content) != 1 || got.Content[0].ToolUse == nil {
		t.Fatalf("重放聚合 content = %#v", got.Content)
	}
	if !reflect.DeepEqual(*got.Content[0].ToolUse, *want.Content[0].ToolUse) {
		t.Errorf("重放聚合 tool_use = %#v, want %#v",
			*got.Content[0].ToolUse, *want.Content[0].ToolUse)
	}
}

// ObjectInput 的三态：nil 给 {}，function 原样，custom 给投影。
func TestObjectInputForms(t *testing.T) {
	var nilUse *ToolUse
	if got := nilUse.ObjectInput(); got != "{}" {
		t.Errorf("nil = %q", got)
	}
	fn := &ToolUse{Input: `{"a":1}`}
	if got := fn.ObjectInput(); got != `{"a":1}` {
		t.Errorf("function = %q", got)
	}
	cu := &ToolUse{Kind: ToolCustom, InputText: `he said "hi"`}
	if got := cu.ObjectInput(); got != `{"input":"he said \"hi\""}` {
		t.Errorf("custom = %q", got)
	}
}
