package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 流式编码的畸形入参：input_json_delta 一旦发出就不可改写，原文只能
// 透传（不延迟流），但 Notes 必须报出——否则客户端把一次参数损坏的
// 调用当正常完成存进历史。块正常关闭与流被掐断两条路都要报。
func TestStreamEncoderMalformedArgsReported(t *testing.T) {
	run := func(t *testing.T, withBlockStop bool) []string {
		t.Helper()
		enc := newStreamEncoder()
		feed := func(ev ir.Event) {
			if _, err := enc.Encode(ev); err != nil {
				t.Fatalf("encode %v: %v", ev.Type, err)
			}
		}
		feed(ir.Event{Type: ir.EvMessageStart, MessageID: "m1", Model: "m"})
		feed(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
			Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "t1", Name: "f"}}})
		feed(ir.Event{Type: ir.EvToolInput, Index: 0, Text: `{"a": 1`})
		if withBlockStop {
			feed(ir.Event{Type: ir.EvBlockStop, Index: 0})
		}
		enc.Finish()
		return enc.Notes()
	}
	for _, withBlockStop := range []bool{true, false} {
		notes := run(t, withBlockStop)
		if !anyNoteHas(notes, "preserved 1 malformed tool call argument(s) as raw text") {
			t.Errorf("withBlockStop=%v 畸形入参未报：%v", withBlockStop, notes)
		}
	}
}

// 对照组：合法对象入参不报。零增量入参（关块补 "{}"）也不报——
// 那是无参调用的正常形态，不是畸形。
func TestStreamEncoderValidArgsSilent(t *testing.T) {
	run := func(t *testing.T, delta string) []string {
		t.Helper()
		enc := newStreamEncoder()
		feed := func(ev ir.Event) {
			if _, err := enc.Encode(ev); err != nil {
				t.Fatalf("encode %v: %v", ev.Type, err)
			}
		}
		feed(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
			Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "t1", Name: "f"}}})
		if delta != "" {
			feed(ir.Event{Type: ir.EvToolInput, Index: 0, Text: delta})
		}
		feed(ir.Event{Type: ir.EvBlockStop, Index: 0})
		feed(ir.Event{Type: ir.EvMessageStop})
		return enc.Notes()
	}
	for _, delta := range []string{`{"a":1}`, ""} {
		if notes := run(t, delta); anyNoteHas(notes, "malformed tool call") {
			t.Errorf("delta=%q 不该报畸形：%v", delta, notes)
		}
	}
}

// 非流式响应编码：input 是对象槽位，畸形原文挪进 ir.RawArgsKey 保真，
// 响应体保持合法，且 EncodeResponseLossy 报出挪键。
func TestEncodeResponseLossyRewrapsMalformedArgs(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{
		Type:    ir.BlockToolUse,
		ToolUse: &ir.ToolUse{ID: "call_1", Name: "grep", Input: `{"pattern":"x`},
	}}}
	body, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if !strings.Contains(string(body), ir.RawArgsKey) {
		t.Errorf("原文没挪进 %s: %s", ir.RawArgsKey, body)
	}
	if !anyNoteHas(notes, "rewrapped 1 malformed tool call argument(s) into "+ir.RawArgsKey) {
		t.Errorf("挪键未报：%v", notes)
	}
}

// 合法对象入参不触发挪键说明。
func TestEncodeResponseLossyValidArgsSilent(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{
		Type:    ir.BlockToolUse,
		ToolUse: &ir.ToolUse{ID: "call_1", Name: "grep", Input: `{"pattern":"x"}`},
	}}}
	_, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if anyNoteHas(notes, "malformed tool call") {
		t.Errorf("合法入参不该报畸形：%v", notes)
	}
}

func anyNoteHas(notes []string, sub string) bool {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}
