package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// serverToolStream 是带托管工具痕迹的一段上游流：
// server_tool_use（查询串分片）+ web_search_tool_result（完整载荷）+ 正文。
const serverToolStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-5","role":"assistant","usage":{"input_tokens":50}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"weather in Paris\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[{"type":"web_search_result","title":"Paris weather","url":"https://example.com/p","encrypted_content":"enc-blob","page_age":"2026"}]}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"It is sunny."}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":20}}

event: message_stop
data: {"type":"message_stop"}

`

// 同族流式往返：托管工具块必须完整进 IR，查询串经增量通道累积回
// ServerToolUse.Input，web_search_tool_result 的载荷一条不丢。
func TestServerToolStreamDecodesIntoIR(t *testing.T) {
	var a ir.Aggregator
	for _, ev := range feed(t, serverToolStream) {
		a.Add(ev)
	}
	got := a.Response()
	if len(got.Content) != 3 {
		t.Fatalf("content = %#v", got.Content)
	}

	b0 := got.Content[0]
	if b0.Type != ir.BlockServerToolUse || b0.ServerToolUse == nil {
		t.Fatalf("block 0 = %#v", b0)
	}
	if b0.ServerToolUse.ID != "srvtoolu_1" || b0.ServerToolUse.Name != "web_search" {
		t.Errorf("server tool use = %#v", b0.ServerToolUse)
	}
	if b0.ServerToolUse.Input != `{"query":"weather in Paris"}` {
		t.Errorf("input = %q", b0.ServerToolUse.Input)
	}

	b1 := got.Content[1]
	if b1.Type != ir.BlockWebSearchToolResult || b1.WebSearchToolResult == nil {
		t.Fatalf("block 1 = %#v", b1)
	}
	wr := b1.WebSearchToolResult
	if wr.ToolUseID != "srvtoolu_1" || wr.ErrorCode != "" || len(wr.Results) != 1 {
		t.Fatalf("web search result = %#v", wr)
	}
	r := wr.Results[0]
	if r.Title != "Paris weather" || r.URL != "https://example.com/p" ||
		r.Snippet != "enc-blob" || r.PageAge != "2026" {
		t.Errorf("result = %#v", r)
	}

	if got.Content[2].Text != "It is sunny." {
		t.Errorf("text = %q", got.Content[2].Text)
	}
}

// 开启帧自带完整查询串的实现形态：块里的 Input 要清掉并补发增量帧，
// 否则下游编码器按「开启帧入参必为空」的约定把它丢掉。
func TestServerToolFullInputAtBlockStartReEmittedAsDelta(t *testing.T) {
	raw := `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"all at once"}}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

`
	events := feed(t, raw)
	if len(events) < 2 {
		t.Fatalf("events = %#v", events)
	}
	start := events[0]
	if start.Type != ir.EvBlockStart || start.Block == nil ||
		start.Block.ServerToolUse == nil {
		t.Fatalf("first event = %#v", start)
	}
	if start.Block.ServerToolUse.Input != "" {
		t.Errorf("开启帧的 Input 没清空: %q", start.Block.ServerToolUse.Input)
	}
	found := false
	for _, ev := range events {
		if ev.Type == ir.EvToolInput && ev.Index == 0 {
			found = true
			if ev.Text != `{"query":"all at once"}` {
				t.Errorf("补发增量 = %q", ev.Text)
			}
		}
	}
	if !found {
		t.Errorf("没补发 EvToolInput: %#v", events)
	}
}

// content 的 union 形态：错误对象解出 ErrorCode 且 Results 为空，
// 不能被当成「搜索成功但零结果」。
func TestDecodeWebSearchToolResultErrorUnion(t *testing.T) {
	errBody := json.RawMessage(
		`{"type":"web_search_tool_result_error","error_code":"max_uses_exceeded"}`)
	got := decodeWebSearchToolResult("srvtoolu_1", errBody)
	if got.ErrorCode != "max_uses_exceeded" {
		t.Errorf("error_code = %q", got.ErrorCode)
	}
	if len(got.Results) != 0 {
		t.Errorf("错误形态不该有结果: %#v", got.Results)
	}

	arrBody := json.RawMessage(
		`[{"type":"web_search_result","title":"t","url":"u","encrypted_content":"e"}]`)
	got = decodeWebSearchToolResult("srvtoolu_1", arrBody)
	if got.ErrorCode != "" || len(got.Results) != 1 ||
		got.Results[0].Snippet != "e" {
		t.Errorf("结果形态 = %#v", got)
	}

	// 畸形 content 不炸整个请求：解不出就是空结果。
	got = decodeWebSearchToolResult("srvtoolu_1", json.RawMessage(`"garbage"`))
	if got == nil || got.ErrorCode != "" || len(got.Results) != 0 {
		t.Errorf("畸形 content = %#v", got)
	}
}

// 请求侧解码放行：多轮历史里带托管工具痕迹的同族往返是合法输入。
func TestDecodeBlockAcceptsServerToolBlocks(t *testing.T) {
	in := wireBlock{Type: blockServerToolUse, ID: "srvtoolu_1",
		Name: "web_search", Input: json.RawMessage(`{"query":"q"}`)}
	b, ok, err := decodeBlock(in)
	if err != nil || !ok {
		t.Fatalf("decodeBlock: ok=%v err=%v", ok, err)
	}
	if b.Type != ir.BlockServerToolUse || b.ServerToolUse == nil ||
		b.ServerToolUse.Input != `{"query":"q"}` {
		t.Errorf("block = %#v", b)
	}

	in = wireBlock{Type: blockWebSearchToolResult, ToolUseID: "srvtoolu_1",
		Content: json.RawMessage(`{"type":"web_search_tool_result_error","error_code":"x"}`)}
	b, ok, err = decodeBlock(in)
	if err != nil || !ok {
		t.Fatalf("decodeBlock: ok=%v err=%v", ok, err)
	}
	if b.Type != ir.BlockWebSearchToolResult || b.WebSearchToolResult == nil ||
		b.WebSearchToolResult.ErrorCode != "x" {
		t.Errorf("block = %#v", b)
	}
}

// 同族编码回吐：server_tool_use 的 Input 走对象槽位归一化，
// 错误形态的结果块编回 web_search_tool_result_error。
func TestEncodeBlockServerToolRoundtrip(t *testing.T) {
	out, ok, err := encodeBlock(ir.Block{
		Type: ir.BlockServerToolUse,
		ServerToolUse: &ir.ServerToolUse{
			ID: "srvtoolu_1", Name: "web_search", Input: `{"query":"q"}`,
		},
	})
	if err != nil || !ok {
		t.Fatalf("encodeBlock: ok=%v err=%v", ok, err)
	}
	if out.Type != blockServerToolUse || out.ID != "srvtoolu_1" ||
		out.Name != "web_search" {
		t.Errorf("wire = %#v", out)
	}
	if string(out.Input) != `{"query":"q"}` {
		t.Errorf("input = %s", out.Input)
	}
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), `"server_tool_use"`) {
		t.Errorf("wire json = %s", raw)
	}

	// 空 Input 归一成 {}，上游拒收裸 null。
	out, _, err = encodeBlock(ir.Block{
		Type:          ir.BlockServerToolUse,
		ServerToolUse: &ir.ServerToolUse{ID: "s", Name: "web_search"},
	})
	if err != nil {
		t.Fatalf("encodeBlock: %v", err)
	}
	if string(out.Input) != "{}" {
		t.Errorf("空入参 = %s, 应为 {}", out.Input)
	}

	out, ok, err = encodeBlock(ir.Block{
		Type: ir.BlockWebSearchToolResult,
		WebSearchToolResult: &ir.WebSearchToolResult{
			ToolUseID: "srvtoolu_1", ErrorCode: "max_uses_exceeded",
		},
	})
	if err != nil || !ok {
		t.Fatalf("encodeBlock: ok=%v err=%v", ok, err)
	}
	if out.Type != blockWebSearchToolResult || out.ToolUseID != "srvtoolu_1" {
		t.Fatalf("wire = %#v", out)
	}
	if !strings.Contains(string(out.Content), "web_search_tool_result_error") ||
		!strings.Contains(string(out.Content), "max_uses_exceeded") {
		t.Errorf("错误形态 content = %s", out.Content)
	}

	out, _, err = encodeBlock(ir.Block{
		Type: ir.BlockWebSearchToolResult,
		WebSearchToolResult: &ir.WebSearchToolResult{
			ToolUseID: "srvtoolu_1",
			Results: []ir.WebSearchResult{
				{Title: "t", URL: "u", Snippet: "e", PageAge: "2026"},
			},
		},
	})
	if err != nil {
		t.Fatalf("encodeBlock: %v", err)
	}
	s := string(out.Content)
	if !strings.Contains(s, `"encrypted_content":"e"`) ||
		!strings.Contains(s, `"page_age":"2026"`) {
		t.Errorf("结果形态 content = %s", s)
	}
}

// 病态查询串原文透传：Input 是字符串槽位，畸形 JSON 归一进
// _modelsurge_raw_args 包装而不是清空——清空等于伪造无参调用。
func TestEncodeBlockServerToolMalformedInputPreserved(t *testing.T) {
	out, _, err := encodeBlock(ir.Block{
		Type: ir.BlockServerToolUse,
		ServerToolUse: &ir.ServerToolUse{
			ID: "s", Name: "web_search", Input: `{"query":`,
		},
	})
	if err != nil {
		t.Fatalf("encodeBlock: %v", err)
	}
	if !strings.Contains(string(out.Input), "_modelsurge_raw_args") {
		t.Errorf("畸形入参 = %s, 应包进 raw args 槽位", out.Input)
	}
}
