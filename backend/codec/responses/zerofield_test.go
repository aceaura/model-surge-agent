package responses

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 第一个条目的序号是 0，omitempty 会把它整个吞掉，客户端按缺席处理时
// 会把后续 delta 归错条目。
func TestFirstBlockFramesCarryZeroIndexes(t *testing.T) {
	enc := inboundCodec{}.NewStreamEncoder(nil)
	frames := encodeEvent(t, enc, ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
		Type: ir.BlockText,
	}})
	frames = append(frames, encodeEvent(t, enc, ir.Event{
		Type: ir.EvTextDelta, Index: 0, Text: "hi",
	})...)

	want := map[string][]string{
		evOutputItemAdded:  {"output_index"},
		evContentPartAdded: {"output_index", "content_index"},
		evOutputTextDelta:  {"output_index", "content_index"},
	}
	for _, frame := range frames {
		kind, obj := parseFrame(t, frame)
		for _, field := range want[kind] {
			raw, ok := obj[field]
			if !ok {
				t.Errorf("%s is missing %s: %s", kind, field, frame)
				continue
			}
			if string(raw) != "0" {
				t.Errorf("%s has %s = %s, want 0", kind, field, raw)
			}
		}
	}
}

// error 帧不属于任何条目，带上 output_index 会让客户端把它归到第一个条目上。
func TestErrorFrameHasNoIndexFields(t *testing.T) {
	frames := RenderStreamError(&ir.Error{Kind: ir.ErrUpstream, Message: "boom"})
	if len(frames) == 0 {
		t.Fatal("no error frames")
	}
	for _, frame := range frames {
		kind, obj := parseFrame(t, frame)
		if kind != evError {
			continue
		}
		for _, field := range []string{"output_index", "content_index"} {
			if _, ok := obj[field]; ok {
				t.Errorf("error frame carries %s: %s", field, frame)
			}
		}
	}
}

// function_call 条目的 arguments 为空时也必须写出：客户端按键存在与否
// 判断条目是否完整，缺键会被当成解析失败。
func TestFunctionCallItemCarriesEmptyArguments(t *testing.T) {
	enc := inboundCodec{}.NewStreamEncoder(nil)
	frames := encodeEvent(t, enc, ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
		Type:    ir.BlockToolUse,
		ToolUse: &ir.ToolUse{ID: "call_1", Name: "search"},
	}})

	var checked bool
	for _, frame := range frames {
		kind, obj := parseFrame(t, frame)
		if kind != evOutputItemAdded {
			continue
		}
		var item map[string]json.RawMessage
		if err := json.Unmarshal(obj["item"], &item); err != nil {
			t.Fatalf("item: %v", err)
		}
		for _, field := range []string{"call_id", "name", "arguments"} {
			if _, ok := item[field]; !ok {
				t.Errorf("function_call item is missing %s: %s", field, frame)
			}
		}
		if string(item["arguments"]) != `""` {
			t.Errorf("arguments = %s, want an empty string", item["arguments"])
		}
		checked = true
	}
	if !checked {
		t.Fatal("no output_item.added frame")
	}
}

// message 条目的 content 必须是 []：nil 的 json.RawMessage 会写出 null，
// 严格客户端把 null 当类型错误，比字段缺席更糟。
func TestMessageItemContentIsArrayNotNull(t *testing.T) {
	body, err := inboundCodec{}.EncodeResponse(&ir.Response{
		Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if strings.Contains(string(body), `"content":null`) {
		t.Errorf("content is null: %s", body)
	}
}

// reasoning 条目不该带 function_call 的必填键：那是本协议里不存在的形状。
func TestReasoningItemHasNoCallFields(t *testing.T) {
	enc := inboundCodec{}.NewStreamEncoder(nil)
	frames := encodeEvent(t, enc, ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
		Type: ir.BlockThinking, Thinking: &ir.Thinking{},
	}})
	for _, frame := range frames {
		kind, obj := parseFrame(t, frame)
		if kind != evOutputItemAdded {
			continue
		}
		var item map[string]json.RawMessage
		if err := json.Unmarshal(obj["item"], &item); err != nil {
			t.Fatalf("item: %v", err)
		}
		for _, field := range []string{"call_id", "name", "arguments"} {
			if _, ok := item[field]; ok {
				t.Errorf("reasoning item carries %s: %s", field, frame)
			}
		}
	}
}

// 出站请求体必须逐字节不变：上游的 prompt cache 按前缀比对，
// 响应侧补出的空字段一旦漏进请求体，缓存会全部失效。
func TestOutboundRequestBytesUnchanged(t *testing.T) {
	req := &ir.Request{
		Model: "m",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
			{Role: ir.RoleAssistant, Content: []ir.Block{
				{Type: ir.BlockThinking, Thinking: &ir.Thinking{Text: "t"}},
				{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
					ID: "call_1", Name: "s", Input: `{"q":1}`,
				}},
			}},
		},
	}
	want := `{"model":"m","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"t"}]},` +
		`{"type":"function_call","call_id":"call_1","name":"s","arguments":"{\"q\":1}"}` +
		`],"stream":true,"store":false}`

	body, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if string(body) != want {
		t.Errorf("request bytes changed:\n got = %s\nwant = %s", body, want)
	}
}

func parseFrame(t *testing.T, frame []byte) (string, map[string]json.RawMessage) {
	t.Helper()
	var data string
	for _, line := range strings.Split(string(frame), "\n") {
		if rest, ok := strings.CutPrefix(line, "data: "); ok {
			data = rest
		}
	}
	if data == "" {
		t.Fatalf("frame has no data line: %q", frame)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &obj); err != nil {
		t.Fatalf("frame payload: %v", err)
	}
	var kind string
	if raw, ok := obj["type"]; ok {
		if err := json.Unmarshal(raw, &kind); err != nil {
			t.Fatalf("frame type: %v", err)
		}
	}
	return kind, obj
}
