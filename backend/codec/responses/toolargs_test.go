package responses

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 流式编码的畸形入参：function_call_arguments 增量一旦发出就不可改写，
// 原文透传（不延迟流），但 Notes 必须报出不可安全执行——否则客户端把
// 一次参数损坏的调用当正常完成存进历史。块正常关闭与断流兜底两条路都要报。
func TestStreamEncoderMalformedArgsReported(t *testing.T) {
	run := func(t *testing.T, withBlockStop bool) (string, []string) {
		t.Helper()
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
			Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "c1", Name: "f"}}})
		feed(ir.Event{Type: ir.EvToolInput, Index: 0, Text: `{"a": 1`})
		if withBlockStop {
			feed(ir.Event{Type: ir.EvBlockStop, Index: 0})
		}
		feed(ir.Event{Type: ir.EvMessageStop})
		for _, f := range enc.Finish() {
			wire.Write(f)
		}
		return wire.String(), enc.Notes()
	}
	for _, withBlockStop := range []bool{true, false} {
		wire, notes := run(t, withBlockStop)
		if !strings.Contains(wire, `\"a\": 1`) {
			t.Errorf("withBlockStop=%v 畸形原文未透传：%s", withBlockStop, wire)
		}
		if !anyNoteHas(notes, "preserved 1 malformed tool call argument(s) as raw text") {
			t.Errorf("withBlockStop=%v 畸形入参未报：%v", withBlockStop, notes)
		}
	}
}

// 对照组：合法入参不报畸形。
func TestStreamEncoderValidArgsSilent(t *testing.T) {
	enc := newStreamEncoder()
	feed := func(ev ir.Event) {
		if _, err := enc.Encode(ev); err != nil {
			t.Fatalf("encode %v: %v", ev.Type, err)
		}
	}
	feed(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
		Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "c1", Name: "f"}}})
	feed(ir.Event{Type: ir.EvToolInput, Index: 0, Text: `{"a":1}`})
	feed(ir.Event{Type: ir.EvBlockStop, Index: 0})
	feed(ir.Event{Type: ir.EvMessageStop})
	if notes := enc.Notes(); anyNoteHas(notes, "malformed tool call") {
		t.Errorf("合法入参不该报畸形：%v", notes)
	}
}

// 非流式响应编码：arguments 是字符串槽位，畸形原文照转义嵌入（响应体
// 保持合法），不清空成 {}；EncodeResponseLossy 报出不可安全执行。
func TestEncodeResponseLossyKeepsMalformedArgsRaw(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{
		Type:    ir.BlockToolUse,
		ToolUse: &ir.ToolUse{ID: "c1", Name: "grep", Input: `{"pattern":"x`},
	}}}
	body, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if !strings.Contains(string(body), `{\"pattern\":\"x`) {
		t.Errorf("畸形原文没透传进 arguments: %s", body)
	}
	if strings.Contains(string(body), `"arguments":"{}"`) {
		t.Errorf("残缺入参被清空成空对象，工具会不带参数执行: %s", body)
	}
	if !anyNoteHas(notes, "preserved 1 malformed tool call argument(s) as raw text") {
		t.Errorf("畸形入参未报：%v", notes)
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
