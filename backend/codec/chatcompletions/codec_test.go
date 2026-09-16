package chatcompletions

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 这里测本协议独有的形态：跨协议矩阵只验语义能否抵达，
// 「字符串与数组两种写法都要认」「tool_calls 的 index 必须写出」
// 这类协议内部约定只能在这里测。

func TestDecodeAcceptsBothMaxTokenSpellings(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"legacy max_tokens", `{"model":"m","max_tokens":512,"messages":[]}`, 512},
		{"new max_completion_tokens", `{"model":"m","max_completion_tokens":512,"messages":[]}`, 512},
		// 两个都给时以新写法为准：客户端 SDK 升级期会同时发送。
		{"new wins over legacy", `{"model":"m","max_tokens":1,"max_completion_tokens":512,"messages":[]}`, 512},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, err := DecodeRequest([]byte(c.body))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if req.MaxTokens != c.want {
				t.Errorf("max_tokens = %d, want %d", req.MaxTokens, c.want)
			}
		})
	}
}

func TestDecodeAcceptsStringAndArrayForms(t *testing.T) {
	body := `{
	  "model": "m",
	  "stop": "END",
	  "messages": [
	    {"role":"user","content":"plain"},
	    {"role":"user","content":[{"type":"text","text":"part"}]}
	  ]
	}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(req.StopSequences) != 1 || req.StopSequences[0] != "END" {
		t.Errorf("stop = %v, want [END]", req.StopSequences)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(req.Messages))
	}
	for i, m := range req.Messages {
		if len(m.Content) != 1 || m.Content[0].Type != ir.BlockText {
			t.Errorf("messages[%d] content = %+v, want one text block", i, m.Content)
		}
	}
}

func TestDecodeSplitsDataURIIntoMediaAndPayload(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":[
	  {"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}},
	  {"type":"image_url","image_url":{"url":"https://example.com/a.png"}}
	]}]}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	blocks := req.Messages[0].Content
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2", len(blocks))
	}
	// 内联图片必须拆成 media_type 与 data：Anthropic 与 Gemini 都要求分开给出。
	if got := blocks[0].Image; got.MediaType != "image/png" || got.Data != "QUJD" || got.URL != "" {
		t.Errorf("inline image = %+v, want media/data split", got)
	}
	if got := blocks[1].Image; got.URL != "https://example.com/a.png" || got.Data != "" {
		t.Errorf("remote image = %+v, want url kept as-is", got)
	}
}

func TestAdjacentToolMessagesMergeIntoOneUserTurn(t *testing.T) {
	body := `{"model":"m","messages":[
	  {"role":"assistant","tool_calls":[
	    {"index":0,"id":"a","type":"function","function":{"name":"f","arguments":"{}"}},
	    {"index":1,"id":"b","type":"function","function":{"name":"g","arguments":"{}"}}
	  ]},
	  {"role":"tool","tool_call_id":"a","content":"ra"},
	  {"role":"tool","tool_call_id":"b","content":"rb"}
	]}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// 两条 tool 消息必须并成一条 user 消息：转成 Anthropic 时角色必须交替，
	// 拆成两条会被上游拒绝。
	if len(req.Messages) != 2 {
		t.Fatalf("messages = %d, want 2 (assistant + merged user)", len(req.Messages))
	}
	last := req.Messages[1]
	if last.Role != ir.RoleUser || len(last.Content) != 2 {
		t.Fatalf("last message = %+v, want one user turn with two tool results", last)
	}
	for i, want := range []string{"a", "b"} {
		if last.Content[i].ToolResult.ToolUseID != want {
			t.Errorf("tool_result[%d] id = %q, want %q", i, last.Content[i].ToolResult.ToolUseID, want)
		}
	}
}

func TestEncodeAlwaysRequestsUsage(t *testing.T) {
	body, err := EncodeRequest(&ir.Request{Model: "m"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var w struct {
		Stream        bool `json:"stream"`
		StreamOptions *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !w.Stream {
		t.Error("stream must be true: the data plane only has one upstream decode path")
	}
	// 缺 include_usage 上游就不发 usage 帧，调度层的冷却与配额判断会失真。
	if w.StreamOptions == nil || !w.StreamOptions.IncludeUsage {
		t.Errorf("stream_options.include_usage must be true, got %+v", w.StreamOptions)
	}
}

func TestThinkingBudgetFoldsIntoEffort(t *testing.T) {
	cases := []struct {
		name   string
		budget int
		want   string
	}{
		{"no budget falls back to medium", 0, "medium"},
		{"small budget is low", 2048, "low"},
		{"mid budget is medium", 8192, "medium"},
		{"large budget is high", 32000, "high"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body, err := EncodeRequest(&ir.Request{
				Model:    "m",
				Thinking: &ir.ThinkingConfig{Enabled: true, BudgetTokens: c.budget},
			})
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			var w struct {
				ReasoningEffort string `json:"reasoning_effort"`
			}
			if err := json.Unmarshal(body, &w); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if w.ReasoningEffort != c.want {
				t.Errorf("reasoning_effort = %q, want %q", w.ReasoningEffort, c.want)
			}
		})
	}
}

func TestEncodeKeepsEffortWhenClientGaveOne(t *testing.T) {
	// 客户端给了档位就不要用预算去覆盖它：那是它明确的意图表达。
	body, err := EncodeRequest(&ir.Request{
		Model:    "m",
		Thinking: &ir.ThinkingConfig{Enabled: true, Effort: "low", BudgetTokens: 32000},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(string(body), `"reasoning_effort":"low"`) {
		t.Errorf("client effort must win over the budget-derived one: %s", body)
	}
}

func TestToolCallIndexIsAlwaysWritten(t *testing.T) {
	enc := newStreamEncoder()
	var frames []byte
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "id", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 0, Text: "hi"},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvBlockStart, Index: 1, Block: &ir.Block{
			Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "c1", Name: "f"}}},
		{Type: ir.EvToolInput, Index: 1, Text: `{}`},
		{Type: ir.EvBlockStop, Index: 1},
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
	// 缺 index 会让客户端把分片当成新调用，回合中断。
	if strings.Count(text, `"index":0`) < 1 {
		t.Errorf("tool_calls[].index missing: %s", text)
	}
	// 工具调用是第 1 个块但是第 0 个工具调用：块索引不能直接当工具序号，
	// 前面还有一个文本块。
	if strings.Contains(text, `"tool_calls":[{"index":1`) {
		t.Errorf("tool index must be its own sequence, not the block index: %s", text)
	}
}

func TestToolCallHeaderIsSentOnlyOnce(t *testing.T) {
	enc := newStreamEncoder()
	var frames []byte
	for _, ev := range []ir.Event{
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
			Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "c1", Name: "f"}}},
		{Type: ir.EvToolInput, Index: 0, Text: `{"a":`},
		{Type: ir.EvToolInput, Index: 0, Text: `1}`},
	} {
		out, _ := enc.Encode(ev)
		for _, f := range out {
			frames = append(frames, f...)
		}
	}
	// 重复发 id 会让部分客户端建出两个调用。
	if n := strings.Count(string(frames), `"id":"c1"`); n != 1 {
		t.Errorf("tool call id sent %d times, want exactly 1: %s", n, frames)
	}
}

func TestStreamDecoderAssignsDistinctBlocksPerSlot(t *testing.T) {
	dec := newStreamDecoder()
	frames := []string{
		`{"id":"i","model":"m","choices":[{"index":0,"delta":{"reasoning_content":"think"}}]}`,
		`{"id":"i","model":"m","choices":[{"index":0,"delta":{"content":"say"}}]}`,
		`{"id":"i","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`,
		doneSentinel,
	}
	var agg ir.Aggregator
	for _, f := range frames {
		events, err := dec.Feed("", f)
		if err != nil {
			t.Fatalf("feed %s: %v", f, err)
		}
		for _, ev := range events {
			agg.Add(ev)
		}
	}
	resp := agg.Response()
	// 三个语义槽位必须落进三个不同的块，混进同一块会让内容互相污染。
	if len(resp.Content) != 3 {
		t.Fatalf("content = %d blocks, want 3: %+v", len(resp.Content), resp.Content)
	}
	want := []ir.BlockType{ir.BlockThinking, ir.BlockText, ir.BlockToolUse}
	for i, kind := range want {
		if resp.Content[i].Type != kind {
			t.Errorf("content[%d] = %q, want %q", i, resp.Content[i].Type, kind)
		}
	}
}

func TestUsageAndFinishReasonCollapseIntoOneMessageDelta(t *testing.T) {
	dec := newStreamDecoder()
	frames := []string{
		`{"id":"i","model":"m","choices":[{"index":0,"delta":{"content":"x"}}]}`,
		`{"id":"i","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"i","model":"m","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":7}}`,
		doneSentinel,
	}
	var events []ir.Event
	for _, f := range frames {
		got, err := dec.Feed("", f)
		if err != nil {
			t.Fatalf("feed: %v", err)
		}
		events = append(events, got...)
	}
	// finish_reason 与 usage 来自两帧，但只能产出一个 message_delta：
	// Anthropic 客户端见到第二个会认为出现了第二条消息。
	var deltas int
	for _, ev := range events {
		if ev.Type == ir.EvMessageDelta {
			deltas++
			if ev.StopReason != ir.StopEndTurn {
				t.Errorf("stop_reason = %q, want end_turn", ev.StopReason)
			}
			if ev.Usage == nil || ev.Usage.InputTokens != 5 || ev.Usage.OutputTokens != 7 {
				t.Errorf("usage = %+v, want 5/7", ev.Usage)
			}
		}
	}
	if deltas != 1 {
		t.Errorf("message_delta count = %d, want 1", deltas)
	}
}

func TestFinishSynthesizesTerminationWhenUpstreamCutsOff(t *testing.T) {
	dec := newStreamDecoder()
	if _, err := dec.Feed("", `{"id":"i","model":"m","choices":[{"index":0,"delta":{"content":"x"}}]}`); err != nil {
		t.Fatalf("feed: %v", err)
	}
	// 上游没发 [DONE] 就断了：必须补出终止事件，否则客户端一直等。
	events := dec.Finish()
	var sawStop bool
	for _, ev := range events {
		if ev.Type == ir.EvMessageStop {
			sawStop = true
		}
	}
	if !sawStop {
		t.Errorf("Finish must synthesize message_stop, got %+v", events)
	}
	// 已经补过就不能再补一次。
	if again := dec.Finish(); len(again) != 0 {
		t.Errorf("second Finish must be empty, got %+v", again)
	}
}

func TestBothCacheFieldNamesAreRead(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"openai style", `{"id":"i","choices":[],"usage":{"prompt_tokens":10,"prompt_tokens_details":{"cached_tokens":4}}}`},
		{"deepseek style", `{"id":"i","choices":[],"usage":{"prompt_tokens":10,"prompt_cache_hit_tokens":4}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, err := DecodeResponse([]byte(c.body))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Usage.CacheReadTokens != 4 {
				t.Errorf("cache_read = %d, want 4", resp.Usage.CacheReadTokens)
			}
		})
	}
}

func TestStreamErrorIsFollowedByDone(t *testing.T) {
	frames := RenderStreamError(ir.NewError(ir.ErrUpstream, 500, "", "boom"))
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2 (error + done)", len(frames))
	}
	if !strings.Contains(string(frames[0]), "boom") {
		t.Errorf("first frame must carry the message: %s", frames[0])
	}
	// 缺 [DONE] 客户端会认为流还没结束，一直挂着。
	if !strings.Contains(string(frames[1]), doneSentinel) {
		t.Errorf("second frame must be the done sentinel: %s", frames[1])
	}
}

func TestCapsDeclareWhatThisProtocolCannotCarry(t *testing.T) {
	caps := outboundCodec{}.Caps()
	// 声明为真会让编码器保留这些字段，而本协议没有承载它们的位置，
	// 结果是上游报未知字段。
	if caps.ThinkingSig {
		t.Error("ThinkingSig must be false: no field carries a reasoning signature")
	}
	if caps.CacheControl {
		t.Error("CacheControl must be false: no field carries cache_control")
	}
	if caps.TopK {
		t.Error("TopK must be false: not part of the protocol")
	}
}

func TestRegisteredUnderTheProtocolName(t *testing.T) {
	// 协议名必须与上游配置中心的枚举一致，否则 dispatch 返回的
	// target.protocol 查不到 codec，目标会被判成 invalid_model。
	if _, ok := codec.Inbound(codec.ProtocolChatCompletions); !ok {
		t.Error("inbound codec not registered")
	}
	if _, ok := codec.Outbound(codec.ProtocolChatCompletions); !ok {
		t.Error("outbound codec not registered")
	}
}
