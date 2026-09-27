package responses

import (
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次17：跨族终止原因折叠注记的接线（responses 出站）。anthropic 的
// context_window_exceeded 本协议无对应值，renderStatus 塌进 max_output_tokens，
// 成因丢失要报；max_messages / steered 是本族原值，不折不报。流式与非流式同判据。

const foldNote = "rewrote the stop reason"

func TestR17ContextWindowFoldNotedResponses(t *testing.T) {
	// 非流式。
	_, notes, err := inboundCodec{}.EncodeResponseLossy(&ir.Response{
		ID: "r1", Model: "m", StopReason: ir.StopContextWindow})
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if !anyNoteHas(notes, foldNote) {
		t.Errorf("非流式 context_window 折叠未报：%v", notes)
	}
	// 流式。
	enc := newStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopContextWindow}); err != nil {
		t.Fatalf("Encode(delta): %v", err)
	}
	enc.Finish()
	if !anyNoteHas(enc.Notes(), foldNote) {
		t.Errorf("流式 context_window 折叠未报：%v", enc.Notes())
	}
}

// 本族原值 max_messages / steered 不折不报。
func TestR17NativeStopNotNotedResponses(t *testing.T) {
	for _, reason := range []ir.StopReason{ir.StopMaxMessages, ir.StopSteered} {
		_, notes, err := inboundCodec{}.EncodeResponseLossy(&ir.Response{
			ID: "r1", Model: "m", StopReason: reason})
		if err != nil {
			t.Fatalf("EncodeResponseLossy(%s): %v", reason, err)
		}
		if anyNoteHas(notes, foldNote) {
			t.Errorf("非流式本族 %s 被误报折叠：%v", reason, notes)
		}
		enc := newStreamEncoder()
		if _, err := enc.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: reason}); err != nil {
			t.Fatalf("Encode(delta %s): %v", reason, err)
		}
		enc.Finish()
		if anyNoteHas(enc.Notes(), foldNote) {
			t.Errorf("流式本族 %s 误报折叠：%v", reason, enc.Notes())
		}
	}
}
