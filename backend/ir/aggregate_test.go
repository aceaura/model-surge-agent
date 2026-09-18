package ir

import "testing"

func TestAggregateTextAndToolBlocks(t *testing.T) {
	a := &Aggregator{}
	a.Add(Event{Type: EvMessageStart, MessageID: "msg_1", Model: "k3"})
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{Type: BlockText}})
	a.Add(Event{Type: EvTextDelta, Index: 0, Text: "he"})
	a.Add(Event{Type: EvTextDelta, Index: 0, Text: "llo"})
	a.Add(Event{Type: EvBlockStop, Index: 0})
	a.Add(Event{Type: EvBlockStart, Index: 1, Block: &Block{
		Type: BlockToolUse, ToolUse: &ToolUse{ID: "tu_1", Name: "read"},
	}})
	a.Add(Event{Type: EvToolInput, Index: 1, Text: `{"path":`})
	a.Add(Event{Type: EvToolInput, Index: 1, Text: `"a.go"}`})
	a.Add(Event{Type: EvBlockStop, Index: 1})
	a.Add(Event{Type: EvMessageDelta, StopReason: StopToolUse,
		Usage: &Usage{InputTokens: 10, OutputTokens: 5}})
	a.Add(Event{Type: EvMessageStop})

	got := a.Response()
	if got.ID != "msg_1" || got.Model != "k3" {
		t.Errorf("response = %+v", got)
	}
	if len(got.Content) != 2 {
		t.Fatalf("content = %#v", got.Content)
	}
	if got.Content[0].Text != "hello" {
		t.Errorf("text = %q", got.Content[0].Text)
	}
	tu := got.Content[1].ToolUse
	if tu == nil || tu.ID != "tu_1" || tu.Input != `{"path":"a.go"}` {
		t.Errorf("tool use = %#v", tu)
	}
	if got.StopReason != StopToolUse {
		t.Errorf("stop_reason = %q", got.StopReason)
	}
	if got.Usage.InputTokens != 10 || got.Usage.OutputTokens != 5 {
		t.Errorf("usage = %+v", got.Usage)
	}
}

// 上游给了普通结束但内容里确有工具调用：终止原因必须改判，
// 否则客户端不会去执行工具，整个回合悄悄断在这里。
func TestToolUseBlockForcesToolUseStopReason(t *testing.T) {
	a := &Aggregator{}
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{
		Type: BlockToolUse, ToolUse: &ToolUse{ID: "tu_1", Name: "read"},
	}})
	a.Add(Event{Type: EvToolInput, Index: 0, Text: `{"path":"a.go"}`})
	a.Add(Event{Type: EvMessageDelta, StopReason: StopEndTurn})

	if got := a.Response().StopReason; got != StopToolUse {
		t.Errorf("stop_reason = %q, want %q", got, StopToolUse)
	}
}

// 被拦截的工具调用不该被执行，所以 content_filter 不改判。
func TestContentFilterSurvivesToolUseBlocks(t *testing.T) {
	a := &Aggregator{}
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{
		Type: BlockToolUse, ToolUse: &ToolUse{ID: "tu_1", Name: "read"},
	}})
	a.Add(Event{Type: EvMessageDelta, StopReason: StopContentFilter})

	if got := a.Response().StopReason; got != StopContentFilter {
		t.Errorf("stop_reason = %q, want %q", got, StopContentFilter)
	}
}

// 没有工具调用就不改判，免得纯文本回答被说成要执行工具。
func TestStopReasonUntouchedWithoutToolUse(t *testing.T) {
	a := &Aggregator{}
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{Type: BlockText}})
	a.Add(Event{Type: EvTextDelta, Index: 0, Text: "hi"})
	a.Add(Event{Type: EvMessageDelta, StopReason: StopEndTurn})

	if got := a.Response().StopReason; got != StopEndTurn {
		t.Errorf("stop_reason = %q, want %q", got, StopEndTurn)
	}
}

func TestAggregateThinkingWithSignature(t *testing.T) {
	a := &Aggregator{}
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{Type: BlockThinking}})
	a.Add(Event{Type: EvThinkingDelta, Index: 0, Text: "step 1"})
	a.Add(Event{Type: EvSigDelta, Index: 0, Text: "sig"})
	a.Add(Event{Type: EvSigDelta, Index: 0, Text: "nature"})
	a.Add(Event{Type: EvBlockStop, Index: 0})

	got := a.Response()
	if len(got.Content) != 1 || got.Content[0].Thinking == nil {
		t.Fatalf("content = %#v", got.Content)
	}
	th := got.Content[0].Thinking
	if th.Text != "step 1" || th.Signature != "signature" {
		t.Errorf("thinking = %+v", th)
	}
}

// 上游的块索引不保证连续（responses 的 output_index 会跳号），
// 输出顺序必须按首次出现顺序而非索引大小。
func TestBlockOrderFollowsFirstAppearance(t *testing.T) {
	a := &Aggregator{}
	a.Add(Event{Type: EvBlockStart, Index: 7, Block: &Block{Type: BlockText}})
	a.Add(Event{Type: EvTextDelta, Index: 7, Text: "first"})
	a.Add(Event{Type: EvBlockStart, Index: 2, Block: &Block{Type: BlockText}})
	a.Add(Event{Type: EvTextDelta, Index: 2, Text: "second"})

	got := a.Response()
	if len(got.Content) != 2 {
		t.Fatalf("content = %#v", got.Content)
	}
	if got.Content[0].Text != "first" || got.Content[1].Text != "second" {
		t.Errorf("order = %q, %q", got.Content[0].Text, got.Content[1].Text)
	}
}

// 上游漏发 block_start 时不能静默丢内容，按 delta 类型补开块。
func TestDeltaWithoutBlockStartOpensBlock(t *testing.T) {
	a := &Aggregator{}
	a.Add(Event{Type: EvTextDelta, Index: 0, Text: "orphan"})

	got := a.Response()
	if len(got.Content) != 1 || got.Content[0].Type != BlockText || got.Content[0].Text != "orphan" {
		t.Errorf("content = %#v", got.Content)
	}
}

// usage 可能分散在 message_start（input）与 message_delta（output）两帧，
// 也可能被重复发送累计值，逐字段取大值两种情况都对。
func TestUsageMergesAcrossFrames(t *testing.T) {
	a := &Aggregator{}
	a.Add(Event{Type: EvMessageStart, Usage: &Usage{InputTokens: 100}})
	a.Add(Event{Type: EvMessageDelta, Usage: &Usage{OutputTokens: 20}})
	a.Add(Event{Type: EvMessageDelta, Usage: &Usage{OutputTokens: 35, CacheReadTokens: 8}})

	got := a.Response()
	if got.Usage.InputTokens != 100 {
		t.Errorf("input = %d, must survive later frames that omit it", got.Usage.InputTokens)
	}
	if got.Usage.OutputTokens != 35 || got.Usage.CacheReadTokens != 8 {
		t.Errorf("usage = %+v", got.Usage)
	}
}

// 入参完整就不是截断。
func TestIncompleteToolsIgnoresValidInput(t *testing.T) {
	a := &Aggregator{}
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{
		Type: BlockToolUse, ToolUse: &ToolUse{ID: "tu_1", Name: "read"},
	}})
	a.Add(Event{Type: EvToolInput, Index: 0, Text: `{"path":"a.go"}`})
	if got := a.IncompleteTools(); len(got) != 0 {
		t.Fatalf("IncompleteTools = %v, want none", got)
	}
}

// 入参发到一半断流：这种响应不能当成功交给客户端。
func TestIncompleteToolsReportsTruncatedInput(t *testing.T) {
	a := &Aggregator{}
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{
		Type: BlockToolUse, ToolUse: &ToolUse{ID: "tu_1", Name: "read"},
	}})
	a.Add(Event{Type: EvToolInput, Index: 0, Text: `{"path":`})
	got := a.IncompleteTools()
	if len(got) != 1 || got[0] != "tu_1" {
		t.Fatalf("IncompleteTools = %v, want [tu_1]", got)
	}
}

// 上游本就没给入参：无参调用是合法形态，不能误报成截断。
func TestIncompleteToolsIgnoresCallWithoutInput(t *testing.T) {
	a := &Aggregator{}
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{
		Type: BlockToolUse, ToolUse: &ToolUse{ID: "tu_1", Name: "now"},
	}})
	if got := a.IncompleteTools(); len(got) != 0 {
		t.Fatalf("IncompleteTools = %v, a call with no arguments is legitimate", got)
	}
}

// id 缺失时退回用工具名作标签，光报一个块号没法诊断。
func TestIncompleteToolsFallsBackToToolName(t *testing.T) {
	a := &Aggregator{}
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{
		Type: BlockToolUse, ToolUse: &ToolUse{Name: "read"},
	}})
	a.Add(Event{Type: EvToolInput, Index: 0, Text: `{"path":`})
	got := a.IncompleteTools()
	if len(got) != 1 || got[0] != "read" {
		t.Fatalf("IncompleteTools = %v, want [read]", got)
	}
}

// 残缺工具入参属于不能补闭合的一类。
func TestUnsafeToCloseCoversTruncatedToolInput(t *testing.T) {
	a := &Aggregator{}
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{
		Type: BlockToolUse, ToolUse: &ToolUse{ID: "tu_1", Name: "read"},
	}})
	a.Add(Event{Type: EvToolInput, Index: 0, Text: `{"path":`})
	got := a.UnsafeToClose()
	if len(got) != 1 || got[0] != "tu_1" {
		t.Fatalf("UnsafeToClose = %v, want [tu_1]", got)
	}
}

// 推理块还开着且没拿到签名：回传给上游会被判伪造而整轮拒收。
func TestUnsafeToCloseCoversUnsignedOpenThinking(t *testing.T) {
	a := &Aggregator{}
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{Type: BlockThinking}})
	a.Add(Event{Type: EvThinkingDelta, Index: 0, Text: "half a thought"})
	got := a.UnsafeToClose()
	if len(got) != 1 || got[0] != "thinking block 0" {
		t.Fatalf("UnsafeToClose = %v, want the open unsigned thinking block", got)
	}
}

// 拿到签名就算说完了，即便上游漏发 block_stop。
func TestUnsafeToCloseAcceptsSignedThinking(t *testing.T) {
	a := &Aggregator{}
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{Type: BlockThinking}})
	a.Add(Event{Type: EvThinkingDelta, Index: 0, Text: "a thought"})
	a.Add(Event{Type: EvSigDelta, Index: 0, Text: "sig"})
	if got := a.UnsafeToClose(); len(got) != 0 {
		t.Fatalf("UnsafeToClose = %v, a signed block is complete", got)
	}
}

// 收到 block_stop 就是正常闭合，不该因为缺签名被判成不安全。
func TestUnsafeToCloseIgnoresClosedThinking(t *testing.T) {
	a := &Aggregator{}
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{Type: BlockThinking}})
	a.Add(Event{Type: EvThinkingDelta, Index: 0, Text: "a thought"})
	a.Add(Event{Type: EvBlockStop, Index: 0})
	if got := a.UnsafeToClose(); len(got) != 0 {
		t.Fatalf("UnsafeToClose = %v, a closed block is complete", got)
	}
}

// 未闭合的纯文本块是可接受的截断：半句话由 max_tokens 表达，不必报错。
func TestUnsafeToCloseIgnoresOpenTextBlock(t *testing.T) {
	a := &Aggregator{}
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{Type: BlockText}})
	a.Add(Event{Type: EvTextDelta, Index: 0, Text: "half a sen"})
	if got := a.UnsafeToClose(); len(got) != 0 {
		t.Fatalf("UnsafeToClose = %v, a truncated sentence is acceptable", got)
	}
}

func TestEmptyStreamYieldsEmptyContent(t *testing.T) {
	var a Aggregator
	got := a.Response()
	if got.Content == nil || len(got.Content) != 0 {
		t.Errorf("content = %#v, want empty non-nil slice", got.Content)
	}
}
