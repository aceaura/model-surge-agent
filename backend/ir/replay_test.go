package ir

import (
	"reflect"
	"testing"
)

func types(events []Event) []EventType {
	out := make([]EventType, 0, len(events))
	for _, e := range events {
		out = append(out, e.Type)
	}
	return out
}

func TestResponseEventsOnNil(t *testing.T) {
	if got := ResponseEvents(nil); got != nil {
		t.Errorf("events = %#v", got)
	}
}

func TestEmptyResponseStillBracketsTheMessage(t *testing.T) {
	got := ResponseEvents(&Response{ID: "msg_1", Model: "m", StopReason: StopEndTurn})
	want := []EventType{EvMessageStart, EvMessageDelta, EvMessageStop}
	if !reflect.DeepEqual(types(got), want) {
		t.Fatalf("types = %v", types(got))
	}
	if got[0].MessageID != "msg_1" || got[0].Model != "m" {
		t.Errorf("start = %#v", got[0])
	}
	if got[1].StopReason != StopEndTurn {
		t.Errorf("delta = %#v", got[1])
	}
}

func TestUsageRidesOnTheMessageDelta(t *testing.T) {
	u := Usage{InputTokens: 7, OutputTokens: 3, CacheReadTokens: 2, ReasoningTokens: 1}
	got := ResponseEvents(&Response{Usage: u})
	for _, e := range got {
		if e.Type != EvMessageDelta {
			continue
		}
		if e.Usage == nil || *e.Usage != u {
			t.Fatalf("delta usage = %#v", e.Usage)
		}
		return
	}
	t.Fatal("no message_delta")
}

func TestTextBlockCarriesContentInTheDelta(t *testing.T) {
	got := ResponseEvents(&Response{Content: []Block{{Type: BlockText, Text: "hello"}}})
	want := []EventType{EvMessageStart, EvBlockStart, EvTextDelta, EvBlockStop,
		EvMessageDelta, EvMessageStop}
	if !reflect.DeepEqual(types(got), want) {
		t.Fatalf("types = %v", types(got))
	}
	// 正文必须不在 block_start 上：流式编码器不从开块事件读正文。
	if got[1].Block == nil || got[1].Block.Text != "" {
		t.Errorf("block_start = %#v", got[1].Block)
	}
	if got[2].Text != "hello" {
		t.Errorf("delta = %#v", got[2])
	}
}

func TestEmptyTextEmitsNoDelta(t *testing.T) {
	got := ResponseEvents(&Response{Content: []Block{{Type: BlockText}}})
	want := []EventType{EvMessageStart, EvBlockStart, EvBlockStop, EvMessageDelta, EvMessageStop}
	if !reflect.DeepEqual(types(got), want) {
		t.Fatalf("types = %v", types(got))
	}
}

func TestThinkingBlockSplitsTextAndSignature(t *testing.T) {
	got := ResponseEvents(&Response{Content: []Block{{
		Type:     BlockThinking,
		Thinking: &Thinking{Text: "why", Signature: "sig", SignatureFrom: "anthropic"},
	}}})
	want := []EventType{EvMessageStart, EvBlockStart, EvThinkingDelta, EvSigDelta,
		EvBlockStop, EvMessageDelta, EvMessageStop}
	if !reflect.DeepEqual(types(got), want) {
		t.Fatalf("types = %v", types(got))
	}
	if b := got[1].Block; b == nil || b.Thinking == nil ||
		b.Thinking.Text != "" || b.Thinking.Signature != "" {
		t.Errorf("block_start = %#v", got[1].Block)
	}
	// 来源随签名带出：流式编码器手里只有当前帧，判不出同族就会把异族
	// 签名照原样写给客户端。
	if got[3].Text != "sig" || got[3].SignatureFrom != "anthropic" {
		t.Errorf("sig delta = %#v", got[3])
	}
}

func TestRedactedFlagSurvivesOnTheSkeleton(t *testing.T) {
	got := ResponseEvents(&Response{Content: []Block{{
		Type:     BlockThinking,
		Thinking: &Thinking{Redacted: true},
	}}})
	if b := got[1].Block; b == nil || b.Thinking == nil || !b.Thinking.Redacted {
		t.Fatalf("block_start = %#v", got[1].Block)
	}
}

func TestThinkingWithoutPayloadEmitsNoDelta(t *testing.T) {
	got := ResponseEvents(&Response{Content: []Block{{Type: BlockThinking}}})
	want := []EventType{EvMessageStart, EvBlockStart, EvBlockStop, EvMessageDelta, EvMessageStop}
	if !reflect.DeepEqual(types(got), want) {
		t.Fatalf("types = %v", types(got))
	}
}

func TestToolUseKeepsIdentityAndMovesInput(t *testing.T) {
	got := ResponseEvents(&Response{Content: []Block{{
		Type:    BlockToolUse,
		ToolUse: &ToolUse{ID: "call_1", Name: "read", Input: `{"path":"a"}`},
	}}})
	want := []EventType{EvMessageStart, EvBlockStart, EvToolInput, EvBlockStop,
		EvMessageDelta, EvMessageStop}
	if !reflect.DeepEqual(types(got), want) {
		t.Fatalf("types = %v", types(got))
	}
	b := got[1].Block
	if b == nil || b.ToolUse == nil || b.ToolUse.ID != "call_1" ||
		b.ToolUse.Name != "read" || b.ToolUse.Input != "" {
		t.Errorf("block_start = %#v", b)
	}
	if got[2].Text != `{"path":"a"}` {
		t.Errorf("input delta = %#v", got[2])
	}
}

func TestMediaAndToolResultBlocksPassThroughWhole(t *testing.T) {
	for _, b := range []Block{
		{Type: BlockImage, Media: &Media{MediaType: "image/png", Data: "AA=="}},
		{Type: BlockToolResult, ToolResult: &ToolResult{ToolUseID: "call_1",
			Content: []Block{{Type: BlockText, Text: "ok"}}}},
	} {
		got := ResponseEvents(&Response{Content: []Block{b}})
		want := []EventType{EvMessageStart, EvBlockStart, EvBlockStop,
			EvMessageDelta, EvMessageStop}
		if !reflect.DeepEqual(types(got), want) {
			t.Fatalf("%s: types = %v", b.Type, types(got))
		}
		if got[1].Block == nil || !reflect.DeepEqual(*got[1].Block, b) {
			t.Errorf("%s: block_start = %#v", b.Type, got[1].Block)
		}
	}
}

func TestBlockIndicesFollowContentOrder(t *testing.T) {
	got := ResponseEvents(&Response{Content: []Block{
		{Type: BlockText, Text: "a"},
		{Type: BlockText, Text: "b"},
		{Type: BlockText, Text: "c"},
	}})
	var starts []int
	for _, e := range got {
		if e.Type == EvBlockStart {
			starts = append(starts, e.Index)
		}
	}
	if !reflect.DeepEqual(starts, []int{0, 1, 2}) {
		t.Fatalf("indices = %v", starts)
	}
}

// 投影喂回 Aggregator 应还原出等价响应：这条性质是「下游不必为非流式
// 上游多开一条分支」的全部依据。
func TestProjectionRoundTripsThroughTheAggregator(t *testing.T) {
	cases := []Response{
		{ID: "msg_1", Model: "m", StopReason: StopEndTurn,
			Content: []Block{{Type: BlockText, Text: "hello"}},
			Usage:   Usage{InputTokens: 5, OutputTokens: 2}},
		{ID: "msg_2", Model: "m", StopReason: StopMaxTokens, Content: []Block{
			{Type: BlockThinking, Thinking: &Thinking{Text: "why", Signature: "s",
				SignatureFrom: "anthropic"}},
			{Type: BlockText, Text: "answer"},
		}},
		{ID: "msg_3", Model: "m", StopReason: StopEndTurn, Content: []Block{
			{Type: BlockImage, Media: &Media{MediaType: "image/png", Data: "AA=="}},
		}},
		{ID: "msg_4", Model: "m", StopReason: StopEndTurn,
			Usage: Usage{CacheReadTokens: 9, CacheWriteTokens: 4, ReasoningTokens: 3}},
	}
	for _, want := range cases {
		var agg Aggregator
		for _, e := range ResponseEvents(&want) {
			agg.Add(e)
		}
		got := agg.Response()
		if got.ID != want.ID || got.Model != want.Model {
			t.Errorf("%s: identity = %q/%q", want.ID, got.ID, got.Model)
		}
		if got.Usage != want.Usage {
			t.Errorf("%s: usage = %#v", want.ID, got.Usage)
		}
		if got.StopReason != want.StopReason {
			t.Errorf("%s: stop = %q", want.ID, got.StopReason)
		}
		wantContent := want.Content
		if wantContent == nil {
			wantContent = []Block{}
		}
		if !reflect.DeepEqual(got.Content, wantContent) {
			t.Errorf("%s: content = %#v", want.ID, got.Content)
		}
	}
}

// 有工具调用时 Aggregator 会把 stop_reason 改判为 tool_use，那是它刻意的
// 行为；这里只断言内容与身份还原，不断言 stop_reason。
func TestToolUseRoundTripsWithReclassifiedStopReason(t *testing.T) {
	want := Response{ID: "msg_5", Model: "m", StopReason: StopEndTurn, Content: []Block{
		{Type: BlockToolUse, ToolUse: &ToolUse{ID: "call_1", Name: "read",
			Input: `{"path":"a"}`}},
	}}
	var agg Aggregator
	for _, e := range ResponseEvents(&want) {
		agg.Add(e)
	}
	got := agg.Response()
	if !reflect.DeepEqual(got.Content, want.Content) {
		t.Fatalf("content = %#v", got.Content)
	}
	if got.StopReason != StopToolUse {
		t.Errorf("stop = %q", got.StopReason)
	}
}

// 入参走增量而非块字段，Aggregator 的截断判定才生效——非流式上游给的
// 入参同样可能是残缺 JSON。
func TestTruncatedToolInputIsStillDetectedAfterProjection(t *testing.T) {
	resp := Response{Content: []Block{
		{Type: BlockToolUse, ToolUse: &ToolUse{ID: "call_1", Name: "read",
			Input: `{"path":`}},
	}}
	var agg Aggregator
	for _, e := range ResponseEvents(&resp) {
		agg.Add(e)
	}
	if got := agg.IncompleteTools(); !reflect.DeepEqual(got, []string{"call_1"}) {
		t.Fatalf("incomplete = %#v", got)
	}
}

// 所有块都收到闭合帧：投影的是一份已完整的响应，不该看起来像断流。
func TestProjectionClosesEveryBlock(t *testing.T) {
	resp := Response{Content: []Block{
		{Type: BlockText, Text: "a"},
		{Type: BlockThinking, Thinking: &Thinking{Text: "b", Signature: "s"}},
	}}
	var agg Aggregator
	for _, e := range ResponseEvents(&resp) {
		agg.Add(e)
	}
	if agg.HasOpenBlocks() {
		t.Error("blocks left open")
	}
	if got := agg.UnsafeToClose(); len(got) != 0 {
		t.Errorf("unsafe = %#v", got)
	}
}

// 投影不得改动传入的响应：调用方可能还要用它记流水。
func TestProjectionDoesNotMutateTheResponse(t *testing.T) {
	resp := Response{Content: []Block{
		{Type: BlockText, Text: "hello"},
		{Type: BlockThinking, Thinking: &Thinking{Text: "why", Signature: "sig"}},
		{Type: BlockToolUse, ToolUse: &ToolUse{ID: "c", Name: "n", Input: "{}"}},
	}}
	before := resp
	before.Content = cloneBlocks(resp.Content)
	_ = ResponseEvents(&resp)
	if !reflect.DeepEqual(resp, before) {
		t.Fatalf("mutated: %#v", resp)
	}
}
