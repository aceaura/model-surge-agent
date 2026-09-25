package responses

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// responses 的 custom_tool_call 条目：入参是自由文本而不是 JSON 参数。
// 此前请求历史里的 custom_tool_call / custom_tool_call_output 条目落进
// appendItem 的 default 分支被硬 400 拒掉——客户端存过的合法历史回放
// 不出去；响应侧则没有 custom 条目形态，自由文本调用会被伪装成
// function_call。

func feedCustomFrames(t *testing.T, d *streamDecoder, evs ...wireStreamEvent) []ir.Event {
	t.Helper()
	var out []ir.Event
	for _, ev := range evs {
		data, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal fixture: %v", err)
		}
		got, err := d.Feed(ev.Type, string(data))
		if err != nil {
			t.Fatalf("feed %s: %v", ev.Type, err)
		}
		out = append(out, got...)
	}
	return out
}

func aggregateEvents(t *testing.T, evs []ir.Event) *ir.Response {
	t.Helper()
	a := &ir.Aggregator{}
	for _, ev := range evs {
		a.Add(ev)
	}
	return a.Response()
}

// 请求历史里的 custom 条目：解码不再 400，同族编码原样往返，
// 自由文本不被包成投影。
func TestCustomToolRequestRoundTrip(t *testing.T) {
	body := `{"model":"m","input":[` +
		`{"type":"custom_tool_call","call_id":"c1","name":"grep","input":"the literal text"}` +
		`]}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	var use *ir.ToolUse
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockToolUse {
				use = b.ToolUse
			}
		}
	}
	if use == nil {
		t.Fatalf("custom_tool_call 没解出 tool_use 块：%+v", req.Messages)
	}
	if use.Kind != ir.ToolCustom {
		t.Errorf("Kind = %q, want custom", use.Kind)
	}
	if use.InputText != "the literal text" {
		t.Errorf("InputText = %q", use.InputText)
	}
	if use.Input != `{"input":"the literal text"}` {
		t.Errorf("Input 投影 = %q", use.Input)
	}

	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"type":"custom_tool_call"`) {
		t.Errorf("同族回写丢了条目类型: %s", s)
	}
	if !strings.Contains(s, `"input":"the literal text"`) {
		t.Errorf("自由文本原文没回写: %s", s)
	}
	if strings.Contains(s, `\"input\":`) {
		t.Errorf("同族回写把自由文本包成了投影: %s", s)
	}
	if strings.Contains(s, `"arguments"`) {
		t.Errorf("custom 条目不该有 arguments 键: %s", s)
	}
}

// custom_tool_call_output 条目：解码成带 Kind 的结果块，同族回写落回
// custom_tool_call_output 而不是 function_call_output。
func TestCustomToolOutputRoundTrip(t *testing.T) {
	body := `{"model":"m","input":[` +
		`{"type":"custom_tool_call_output","call_id":"c1","output":"raw result text"}` +
		`]}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	var res *ir.ToolResult
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockToolResult {
				res = b.ToolResult
			}
		}
	}
	if res == nil {
		t.Fatalf("custom_tool_call_output 没解出 tool_result 块：%+v", req.Messages)
	}
	if res.Kind != ir.ToolCustom {
		t.Errorf("Kind = %q, want custom", res.Kind)
	}
	if res.IsError {
		t.Errorf("普通结果被误判为失败态")
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"type":"custom_tool_call_output"`) {
		t.Errorf("结果条目类型没回写: %s", s)
	}
	if !strings.Contains(s, `"output":"raw result text"`) {
		t.Errorf("结果正文没回写: %s", s)
	}
	if strings.Contains(s, `"function_call_output"`) {
		t.Errorf("custom 结果被配到 function_call_output 条目: %s", s)
	}
	// 失败前缀照样认回来：那是我们上一轮出站写的标记。
	errBody := `{"model":"m","input":[{"type":"custom_tool_call_output","call_id":"c1","output":` +
		mustMarshal(t, codec.ToolErrorPrefix()+"boom") + `}]}`
	req2, err := DecodeRequest([]byte(errBody))
	if err != nil {
		t.Fatalf("DecodeRequest(errBody): %v", err)
	}
	res2 := req2.Messages[0].Content[0].ToolResult
	if !res2.IsError {
		t.Errorf("custom 输出里的失败前缀没认回来")
	}
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// 流式解码：added + input.delta 序列解出 Kind=custom 的调用块，
// 聚合终态 InputText 是自由文本、Input 是投影。
func TestCustomToolStreamDecode(t *testing.T) {
	d := newStreamDecoder()
	evs := feedCustomFrames(t, d,
		wireStreamEvent{Type: evCreated, Response: &wireResponse{ID: "r1", Status: "in_progress"}},
		wireStreamEvent{Type: evOutputItemAdded, OutputIndex: 0, Item: &wireRespItem{
			Type: itemCustomToolCall, CallID: "c1", Name: "grep"}},
		wireStreamEvent{Type: evCustomToolInputDelta, OutputIndex: 0, Delta: "the lite"},
		wireStreamEvent{Type: evCustomToolInputDelta, OutputIndex: 0, Delta: "ral text"},
		wireStreamEvent{Type: evOutputItemDone, OutputIndex: 0, Item: &wireRespItem{
			Type: itemCustomToolCall, CallID: "c1", Name: "grep", Input: "the literal text"}},
		wireStreamEvent{Type: evCompleted, Response: &wireResponse{ID: "r1", Status: "completed"}},
	)
	resp := aggregateEvents(t, evs)
	if len(resp.Content) != 1 || resp.Content[0].ToolUse == nil {
		t.Fatalf("content = %+v", resp.Content)
	}
	u := resp.Content[0].ToolUse
	if u.Kind != ir.ToolCustom {
		t.Errorf("Kind = %q, want custom", u.Kind)
	}
	if u.InputText != "the literal text" {
		t.Errorf("InputText = %q", u.InputText)
	}
	if u.Input != `{"input":"the literal text"}` {
		t.Errorf("Input 投影 = %q", u.Input)
	}
	if resp.StopReason != ir.StopToolUse {
		t.Errorf("StopReason = %q, want tool_use", resp.StopReason)
	}
}

// done-only 上游：没有任何增量帧，块在 output_item.done 上补开，
// 形态仍须是 custom（backfill 自己开块时不知道形态）。
func TestCustomToolStreamDecodeDoneOnly(t *testing.T) {
	d := newStreamDecoder()
	evs := feedCustomFrames(t, d,
		wireStreamEvent{Type: evCreated, Response: &wireResponse{ID: "r1", Status: "in_progress"}},
		wireStreamEvent{Type: evOutputItemDone, OutputIndex: 0, Item: &wireRespItem{
			Type: itemCustomToolCall, CallID: "c1", Name: "grep", Input: "all at once"}},
		wireStreamEvent{Type: evCompleted, Response: &wireResponse{ID: "r1", Status: "completed"}},
	)
	resp := aggregateEvents(t, evs)
	if len(resp.Content) != 1 || resp.Content[0].ToolUse == nil {
		t.Fatalf("content = %+v", resp.Content)
	}
	u := resp.Content[0].ToolUse
	if u.Kind != ir.ToolCustom || u.InputText != "all at once" {
		t.Errorf("done-only 补开丢了形态或文本：Kind=%q InputText=%q", u.Kind, u.InputText)
	}
	if u.Input != `{"input":"all at once"}` {
		t.Errorf("Input 投影 = %q", u.Input)
	}
}

// 增量先于 added 帧（上游漏发 added）：块在 custom input.delta 上补开，
// 形态由 customKinds 记账带上。
func TestCustomToolStreamDecodeDeltaOnly(t *testing.T) {
	d := newStreamDecoder()
	evs := feedCustomFrames(t, d,
		wireStreamEvent{Type: evCreated, Response: &wireResponse{ID: "r1", Status: "in_progress"}},
		wireStreamEvent{Type: evCustomToolInputDelta, OutputIndex: 0, Delta: "free text"},
		wireStreamEvent{Type: evCustomToolInputDone, OutputIndex: 0, Input: "free text"},
		wireStreamEvent{Type: evCompleted, Response: &wireResponse{ID: "r1", Status: "completed"}},
	)
	resp := aggregateEvents(t, evs)
	if len(resp.Content) != 1 || resp.Content[0].ToolUse == nil {
		t.Fatalf("content = %+v", resp.Content)
	}
	u := resp.Content[0].ToolUse
	if u.Kind != ir.ToolCustom || u.InputText != "free text" {
		t.Errorf("delta 补开丢了形态或文本：Kind=%q InputText=%q", u.Kind, u.InputText)
	}
}

// 非流式响应解码：custom_tool_call 条目进 IR 带形态，stop_reason 判
// tool_use（漏判会让客户端不去执行工具）。
func TestCustomToolNonStreamDecode(t *testing.T) {
	body := `{"id":"r1","status":"completed","output":[` +
		`{"type":"custom_tool_call","call_id":"c1","name":"grep","input":"free text"}` +
		`]}`
	resp, err := DecodeResponse([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.StopReason != ir.StopToolUse {
		t.Errorf("StopReason = %q, want tool_use", resp.StopReason)
	}
	u := resp.Content[0].ToolUse
	if u.Kind != ir.ToolCustom || u.InputText != "free text" {
		t.Errorf("Kind=%q InputText=%q", u.Kind, u.InputText)
	}
	if u.Input != `{"input":"free text"}` {
		t.Errorf("Input 投影 = %q", u.Input)
	}
}

// 客户端流式编码：增量走 custom_tool_call_input.delta 而不是
// function_call_arguments.delta，终态条目是 custom_tool_call 且 input 键
// 承载自由文本；自由文本不得被误报成畸形参数。
func TestCustomToolStreamEncode(t *testing.T) {
	enc := newStreamEncoder()
	var wire strings.Builder
	feed := func(ev ir.Event) {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("encode %v: %v", ev.Type, err)
		}
		for _, f := range frames {
			wire.Write(f)
		}
	}
	feed(ir.Event{Type: ir.EvMessageStart, MessageID: "r1", Model: "m"})
	feed(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
		Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "c1", Name: "grep", Kind: ir.ToolCustom}}})
	feed(ir.Event{Type: ir.EvToolInput, Index: 0, Text: "not json at all"})
	feed(ir.Event{Type: ir.EvBlockStop, Index: 0})
	feed(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopToolUse})
	feed(ir.Event{Type: ir.EvMessageStop})
	for _, f := range enc.Finish() {
		wire.Write(f)
	}
	s := wire.String()
	if !strings.Contains(s, `response.custom_tool_call_input.delta`) {
		t.Errorf("增量没走 custom_tool_call_input.delta: %s", s)
	}
	if strings.Contains(s, `response.function_call_arguments`) {
		t.Errorf("custom 增量走了 function_call_arguments 事件: %s", s)
	}
	if !strings.Contains(s, `"type":"custom_tool_call"`) {
		t.Errorf("终态条目类型不是 custom_tool_call: %s", s)
	}
	if !strings.Contains(s, `"input":"not json at all"`) {
		t.Errorf("终态条目缺自由文本 input: %s", s)
	}
	if strings.Contains(s, `"arguments"`) {
		t.Errorf("custom 条目带出了 arguments 键: %s", s)
	}
	if anyNoteHas(enc.Notes(), "malformed tool call") {
		t.Errorf("自由文本被误报成畸形参数：%v", enc.Notes())
	}
}

// 客户端非流式编码：custom 块回 custom_tool_call 条目、input 键承载
// InputText 原文（不是投影）。
func TestCustomToolNonStreamEncode(t *testing.T) {
	resp := &ir.Response{StopReason: ir.StopToolUse, Content: []ir.Block{{
		Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
			ID: "c1", Name: "grep", Kind: ir.ToolCustom,
			InputText: "free text", Input: `{"input":"free text"}`,
		},
	}}}
	body, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	s := string(body)
	if !strings.Contains(s, `"type":"custom_tool_call"`) {
		t.Errorf("条目类型不对: %s", s)
	}
	if !strings.Contains(s, `"input":"free text"`) {
		t.Errorf("input 键没承载原文: %s", s)
	}
	if strings.Contains(s, `\"input\":`) {
		t.Errorf("投影被写回本族: %s", s)
	}
	if strings.Contains(s, `"arguments"`) {
		t.Errorf("custom 条目带出了 arguments 键: %s", s)
	}
}

// requiredIndexFields 守卫：custom 入参增量帧在 output_index=0 时
// 也必须写出序号键。
func TestCustomToolDeltaFrameHasOutputIndex(t *testing.T) {
	enc := newStreamEncoder()
	var wire strings.Builder
	for _, ev := range []ir.Event{
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
			Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "c1", Name: "g", Kind: ir.ToolCustom}}},
		{Type: ir.EvToolInput, Index: 0, Text: "x"},
	} {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("encode %v: %v", ev.Type, err)
		}
		for _, f := range frames {
			wire.Write(f)
		}
	}
	s := wire.String()
	i := strings.Index(s, evCustomToolInputDelta)
	if i < 0 {
		t.Fatalf("没有 custom 增量帧: %s", s)
	}
	frame := s[i:]
	if !strings.Contains(frame, `"output_index":0`) {
		t.Errorf("custom 增量帧缺 output_index 零值: %s", frame)
	}
}
