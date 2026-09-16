package responses

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 这里测本协议独有的形态：条目拍平/合并、两级序号折成一级块索引、
// 终止帧必须带完整 response 对象。跨协议矩阵只验语义抵达，测不到这些。

func TestDecodeAcceptsStringAndItemArrayInput(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"bare string", `{"model":"m","input":"hello"}`},
		{"item array", `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`},
		{"item without type", `{"model":"m","input":[{"role":"user","content":"hello"}]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, err := DecodeRequest([]byte(c.body))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(req.Messages) != 1 || req.Messages[0].Role != ir.RoleUser {
				t.Fatalf("messages = %+v, want one user message", req.Messages)
			}
			if got := req.Messages[0].Content; len(got) != 1 || got[0].Text != "hello" {
				t.Errorf("content = %+v, want one text block %q", got, "hello")
			}
		})
	}
}

func TestConsecutiveItemsMergeIntoOneMessage(t *testing.T) {
	// 一个逻辑回合在本协议里拆成多个条目。逐条建消息会产出一串单块消息，
	// 转成 Anthropic 时因为角色必须交替而被拒。
	body := `{"model":"m","input":[
	  {"type":"reasoning","summary":[{"type":"summary_text","text":"think"}]},
	  {"type":"message","role":"assistant","content":[{"type":"output_text","text":"say"}]},
	  {"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}
	]}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("messages = %d, want 1 merged assistant turn: %+v", len(req.Messages), req.Messages)
	}
	m := req.Messages[0]
	if m.Role != ir.RoleAssistant {
		t.Errorf("role = %q, want assistant", m.Role)
	}
	want := []ir.BlockType{ir.BlockThinking, ir.BlockText, ir.BlockToolUse}
	if len(m.Content) != len(want) {
		t.Fatalf("content = %d blocks, want %d: %+v", len(m.Content), len(want), m.Content)
	}
	for i, kind := range want {
		if m.Content[i].Type != kind {
			t.Errorf("content[%d] = %q, want %q", i, m.Content[i].Type, kind)
		}
	}
}

func TestEncodedItemOrderPutsReasoningAndResultsFirst(t *testing.T) {
	req := &ir.Request{
		Model: "m",
		Messages: []ir.Message{{
			Role: ir.RoleAssistant,
			Content: []ir.Block{
				{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
					ToolUseID: "c0", Content: []ir.Block{{Type: ir.BlockText, Text: "prior"}}}},
				{Type: ir.BlockText, Text: "say"},
				{Type: ir.BlockThinking, Thinking: &ir.Thinking{Text: "think"}},
				{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "c1", Name: "f", Input: "{}"}},
			},
		}},
	}
	body, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var w struct {
		Input []struct {
			Type string `json:"type"`
		} `json:"input"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// 顺序是协议要求：工具结果与推理必须排在它们所属的输出之前。
	want := []string{itemFunctionCallOutput, itemReasoning, itemMessage, itemFunctionCall}
	if len(w.Input) != len(want) {
		t.Fatalf("input = %d items, want %d: %s", len(w.Input), len(want), body)
	}
	for i, kind := range want {
		if w.Input[i].Type != kind {
			t.Errorf("input[%d] = %q, want %q", i, w.Input[i].Type, kind)
		}
	}
}

func TestEncodeUsesRoleAppropriatePartType(t *testing.T) {
	req := &ir.Request{
		Model: "m",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "q"}}},
			{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: "a"}}},
		},
	}
	body, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// 用错 part 类型会被上游按格式错误拒掉。
	if !strings.Contains(string(body), partInputText) {
		t.Errorf("user text must use %s: %s", partInputText, body)
	}
	if !strings.Contains(string(body), partOutputText) {
		t.Errorf("assistant text must use %s: %s", partOutputText, body)
	}
}

func TestEncodeDisablesUpstreamStorage(t *testing.T) {
	body, err := EncodeRequest(&ir.Request{Model: "m"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var w struct {
		Store  *bool `json:"store"`
		Stream bool  `json:"stream"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// 换目标重试时各上游的留存状态互不可见，留存会产生看不见的分叉。
	if w.Store == nil || *w.Store {
		t.Errorf("store must be explicitly false, got %v", w.Store)
	}
	if !w.Stream {
		t.Error("stream must be true: the data plane only has one upstream decode path")
	}
}

func TestEncodeRequestsReasoningSummary(t *testing.T) {
	body, err := EncodeRequest(&ir.Request{
		Model:    "m",
		Thinking: &ir.ThinkingConfig{Enabled: true, Effort: "high"},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var w struct {
		Reasoning *wireReasoning `json:"reasoning"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if w.Reasoning == nil || w.Reasoning.Effort != "high" {
		t.Fatalf("reasoning = %+v, want effort high", w.Reasoning)
	}
	// 不请求摘要的话推理内容完全不可见，转回 Anthropic 时 thinking 块是空的。
	if w.Reasoning.Summary == "" {
		t.Error("summary must be requested, otherwise reasoning is invisible downstream")
	}
}

func TestTwoLevelIndicesFoldIntoDistinctBlocks(t *testing.T) {
	// 同一条目内的两个 part 必须成为两个 IR 块：直接拿 output_index
	// 当块索引会把它们混成一块。
	dec := newStreamDecoder()
	frames := []struct{ event, data string }{
		{evCreated, `{"type":"response.created","response":{"id":"r","model":"m"}}`},
		{evOutputItemAdded, `{"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant"}}`},
		{evContentPartAdded, `{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text"}}`},
		{evOutputTextDelta, `{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"first"}`},
		{evContentPartAdded, `{"type":"response.content_part.added","output_index":0,"content_index":1,"part":{"type":"output_text"}}`},
		{evOutputTextDelta, `{"type":"response.output_text.delta","output_index":0,"content_index":1,"delta":"second"}`},
		{evOutputItemDone, `{"type":"response.output_item.done","output_index":0,"item":{"type":"message"}}`},
		{evCompleted, `{"type":"response.completed","response":{"id":"r","model":"m","status":"completed"}}`},
	}
	var agg ir.Aggregator
	for _, f := range frames {
		events, err := dec.Feed(f.event, f.data)
		if err != nil {
			t.Fatalf("feed %s: %v", f.event, err)
		}
		for _, ev := range events {
			agg.Add(ev)
		}
	}
	resp := agg.Response()
	if len(resp.Content) != 2 {
		t.Fatalf("content = %d blocks, want 2: %+v", len(resp.Content), resp.Content)
	}
	if resp.Content[0].Text != "first" || resp.Content[1].Text != "second" {
		t.Errorf("blocks = %q / %q, want first / second",
			resp.Content[0].Text, resp.Content[1].Text)
	}
}

func TestItemDoneClosesOnlyThatItemsBlocks(t *testing.T) {
	dec := newStreamDecoder()
	feed := func(event, data string) []ir.Event {
		out, err := dec.Feed(event, data)
		if err != nil {
			t.Fatalf("feed %s: %v", event, err)
		}
		return out
	}
	feed(evOutputItemAdded, `{"output_index":0,"item":{"type":"reasoning"}}`)
	feed(evReasoningSummaryText, `{"output_index":0,"delta":"t"}`)
	feed(evOutputItemAdded, `{"output_index":1,"item":{"type":"function_call","call_id":"c","name":"f"}}`)
	// 闭合条目 0 时不能连带闭合条目 1：客户端会以为工具调用已完整，
	// 而入参还没到齐。
	events := feed(evOutputItemDone, `{"output_index":0,"item":{"type":"reasoning"}}`)
	if len(events) != 1 || events[0].Type != ir.EvBlockStop || events[0].Index != 0 {
		t.Fatalf("events = %+v, want exactly block_stop on index 0", events)
	}
}

func TestStopReasonIsInferredFromResponseShape(t *testing.T) {
	cases := []struct {
		name string
		body string
		want ir.StopReason
	}{
		{"normal completion", `{"id":"r","status":"completed"}`, ir.StopEndTurn},
		{"truncated by length",
			`{"id":"r","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}`,
			ir.StopMaxTokens},
		{"filtered",
			`{"id":"r","status":"incomplete","incomplete_details":{"reason":"content_filter"}}`,
			ir.StopContentFilter},
		// 本协议没有 stop_reason 字段，工具调用只能靠 output 里有 function_call 推断。
		{"tool call inferred from output",
			`{"id":"r","status":"completed","output":[{"type":"function_call","call_id":"c","name":"f"}]}`,
			ir.StopToolUse},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, err := DecodeResponse([]byte(c.body))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.StopReason != c.want {
				t.Errorf("stop_reason = %q, want %q", resp.StopReason, c.want)
			}
		})
	}
}

func TestTerminalFrameCarriesTheFullResponse(t *testing.T) {
	enc := newStreamEncoder()
	var frames []byte
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "r1", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 0, Text: "answer"},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn,
			Usage: &ir.Usage{InputTokens: 3, OutputTokens: 4}},
		{Type: ir.EvMessageStop},
	} {
		out, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("encode %s: %v", ev.Type, err)
		}
		for _, f := range out {
			frames = append(frames, f...)
		}
	}
	text := string(frames)
	// 客户端 SDK 从终止帧的 response 对象取最终结果，不靠自己拼增量。
	if !strings.Contains(text, evCompleted) {
		t.Fatalf("no completed frame: %s", text)
	}
	tail := text[strings.LastIndex(text, evCompleted):]
	for _, want := range []string{`"answer"`, `"input_tokens":3`, `"output_tokens":4`, `"status":"completed"`} {
		if !strings.Contains(tail, want) {
			t.Errorf("terminal frame missing %s: %s", want, tail)
		}
	}
}

func TestEachItemGetsPairedAddedAndDoneFrames(t *testing.T) {
	enc := newStreamEncoder()
	var frames []byte
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "r1", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 0, Text: "x"},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvMessageStop},
	} {
		out, _ := enc.Encode(ev)
		for _, f := range out {
			frames = append(frames, f...)
		}
	}
	text := string(frames)
	// 帧序不成对，SDK 会认为响应不完整。
	for _, pair := range []string{evOutputItemAdded, evOutputItemDone, evContentPartAdded, evContentPartDone} {
		if !strings.Contains(text, pair) {
			t.Errorf("missing %s: %s", pair, text)
		}
	}
}

func TestFinishClosesUnterminatedItems(t *testing.T) {
	enc := newStreamEncoder()
	// 上游在块开着时断流：Finish 必须闭合它并发终止帧，
	// 否则客户端一直等一个不会来的结束。
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "r", Model: "m"}); err != nil {
		t.Fatalf("encode start: %v", err)
	}
	if _, err := enc.Encode(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "partial"}); err != nil {
		t.Fatalf("encode delta: %v", err)
	}
	var tail []byte
	for _, f := range enc.Finish() {
		tail = append(tail, f...)
	}
	text := string(tail)
	if !strings.Contains(text, evOutputItemDone) {
		t.Errorf("Finish must close the open item: %s", text)
	}
	if !strings.Contains(text, evCompleted) {
		t.Errorf("Finish must emit a terminal frame: %s", text)
	}
	if again := enc.Finish(); len(again) != 0 {
		t.Errorf("second Finish must be empty, got %d frames", len(again))
	}
}

func TestEncryptedReasoningStaysWithinTheFamily(t *testing.T) {
	req := &ir.Request{
		Model: "m",
		Messages: []ir.Message{{Role: ir.RoleAssistant, Content: []ir.Block{
			{Type: ir.BlockThinking, Thinking: &ir.Thinking{
				Text: "t", Signature: "enc-mine", SignatureFrom: Name}},
			{Type: ir.BlockThinking, Thinking: &ir.Thinking{
				Text: "t2", Signature: "enc-theirs", SignatureFrom: "anthropic"}},
		}}},
	}
	body, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	text := string(body)
	// 别家的签名发过来会被上游拒；自家的丢掉会让多轮思考失效。
	if strings.Contains(text, "enc-theirs") {
		t.Errorf("foreign signature must be dropped: %s", text)
	}
	if !strings.Contains(text, "enc-mine") {
		t.Errorf("own signature must survive: %s", text)
	}
}

func TestCapsDeclareNoStopSequences(t *testing.T) {
	// 本协议没有停止序列字段。声明为真会让调用方以为已生效。
	caps := outboundCodec{}.Caps()
	if caps.StopSequences {
		t.Error("StopSequences must be false: the protocol has no such field")
	}
}

func TestRegisteredUnderTheProtocolName(t *testing.T) {
	if _, ok := codec.Inbound(codec.ProtocolResponses); !ok {
		t.Error("inbound codec not registered")
	}
	if _, ok := codec.Outbound(codec.ProtocolResponses); !ok {
		t.Error("outbound codec not registered")
	}
}
