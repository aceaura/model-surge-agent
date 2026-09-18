package chatcompletions

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// decodeStream 把帧喂进解码器，返回聚合响应与解码器说明。
func decodeStream(t *testing.T, frames ...string) (*ir.Response, []string) {
	t.Helper()
	dec := newStreamDecoder()
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
	for _, ev := range dec.Finish() {
		agg.Add(ev)
	}
	var asDecoder codec.StreamDecoder = dec
	notes, ok := asDecoder.(codec.StreamNotes)
	if !ok {
		t.Fatal("stream decoder does not report notes")
	}
	return agg.Response(), notes.Notes()
}

// 探针实证的缺口：同一次调用的分片带着不同 index 到达，
// 只看 index 会拆成两个块，客户端就会重复执行同一个工具。
func TestSameToolIDAcrossIndexesMergesIntoOneBlock(t *testing.T) {
	resp, notes := decodeStream(t,
		`{"id":"i","model":"m","choices":[{"index":0,"delta":{"tool_calls":[`+
			`{"index":0,"id":"dup","type":"function","function":{"name":"f","arguments":"{\"a\":"}}]}}]}`,
		`{"id":"i","model":"m","choices":[{"index":0,"delta":{"tool_calls":[`+
			`{"index":1,"id":"dup","type":"function","function":{"arguments":"1}"}}]}}]}`,
		doneSentinel,
	)

	tools := toolBlocks(resp)
	if len(tools) != 1 {
		t.Fatalf("tool blocks = %d, want 1: %+v", len(tools), tools)
	}
	if tools[0].ToolUse.Input != `{"a":1}` {
		t.Errorf("input = %q, want the two fragments joined", tools[0].ToolUse.Input)
	}
	if !hasMergeNote(notes) {
		t.Errorf("notes = %#v, want a merge note", notes)
	}
}

func TestDistinctToolIDsStayInSeparateBlocks(t *testing.T) {
	resp, notes := decodeStream(t,
		`{"id":"i","model":"m","choices":[{"index":0,"delta":{"tool_calls":[`+
			`{"index":0,"id":"a","type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`,
		`{"id":"i","model":"m","choices":[{"index":0,"delta":{"tool_calls":[`+
			`{"index":1,"id":"b","type":"function","function":{"name":"g","arguments":"{}"}}]}}]}`,
		doneSentinel,
	)

	tools := toolBlocks(resp)
	if len(tools) != 2 {
		t.Fatalf("tool blocks = %d, want 2: %+v", len(tools), tools)
	}
	if hasMergeNote(notes) {
		t.Errorf("notes = %#v, want no merge note", notes)
	}
}

// 空 id 不入索引：否则两个空 id 会互相误合并，
// 把不同调用的入参串成一份非法 JSON。
func TestEmptyToolIDsSplitByIndex(t *testing.T) {
	resp, notes := decodeStream(t,
		`{"id":"i","model":"m","choices":[{"index":0,"delta":{"tool_calls":[`+
			`{"index":0,"type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`,
		`{"id":"i","model":"m","choices":[{"index":0,"delta":{"tool_calls":[`+
			`{"index":1,"type":"function","function":{"name":"g","arguments":"{}"}}]}}]}`,
		doneSentinel,
	)

	tools := toolBlocks(resp)
	if len(tools) != 2 {
		t.Fatalf("tool blocks = %d, want 2: %+v", len(tools), tools)
	}
	if tools[0].ToolUse.ID == tools[1].ToolUse.ID {
		t.Errorf("synthesized ids collide: %q", tools[0].ToolUse.ID)
	}
	if hasMergeNote(notes) {
		t.Errorf("notes = %#v, want no merge note", notes)
	}
}

// 同 index 换 id 仍按两次调用处理：上游复用 index 表示新调用时
// 并进原槽位会把两份入参串成非法 JSON。
func TestReusedIndexWithNewIDStaysSeparate(t *testing.T) {
	resp, _ := decodeStream(t,
		`{"id":"i","model":"m","choices":[{"index":0,"delta":{"tool_calls":[`+
			`{"index":0,"id":"a","type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`,
		`{"id":"i","model":"m","choices":[{"index":0,"delta":{"tool_calls":[`+
			`{"index":0,"id":"b","type":"function","function":{"name":"g","arguments":"{}"}}]}}]}`,
		doneSentinel,
	)

	tools := toolBlocks(resp)
	if len(tools) != 2 {
		t.Fatalf("tool blocks = %d, want 2: %+v", len(tools), tools)
	}
}

func toolBlocks(resp *ir.Response) []ir.Block {
	var out []ir.Block
	for _, b := range resp.Content {
		if b.Type == ir.BlockToolUse && b.ToolUse != nil {
			out = append(out, b)
		}
	}
	return out
}

func hasMergeNote(notes []string) bool {
	for _, n := range notes {
		if strings.Contains(n, "different indexes") {
			return true
		}
	}
	return false
}
