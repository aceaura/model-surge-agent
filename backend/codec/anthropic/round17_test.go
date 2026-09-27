package anthropic

import (
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次17：跨族终止原因折叠注记的接线（anthropic 出站）。responses 的
// max_messages / steered 本协议无对应值，renderStopReason 塌进 max_tokens；
// context_window_exceeded 是本族原值，不折不报。流式与非流式同判据。

const foldNote = "rewrote the stop reason"

func TestR17StopFoldNotedAnthropic(t *testing.T) {
	for _, reason := range []ir.StopReason{ir.StopMaxMessages, ir.StopSteered} {
		// 非流式。
		_, notes, err := inboundCodec{}.EncodeResponseLossy(&ir.Response{
			ID: "msg_1", Model: "m", StopReason: reason})
		if err != nil {
			t.Fatalf("EncodeResponseLossy(%s): %v", reason, err)
		}
		if !hasNote(notes, foldNote) {
			t.Errorf("非流式 %s 折叠未报：%v", reason, notes)
		}
		// 流式：同一判据，不许两条路径口径分叉。
		enc := newStreamEncoder()
		if _, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "msg_1", Model: "m"}); err != nil {
			t.Fatalf("Encode(start): %v", err)
		}
		if _, err := enc.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: reason}); err != nil {
			t.Fatalf("Encode(delta %s): %v", reason, err)
		}
		enc.Finish()
		if !hasNote(enc.Notes(), foldNote) {
			t.Errorf("流式 %s 折叠未报：%v", reason, enc.Notes())
		}
	}
}

// 本族原值 context_window_exceeded 不折不报（anthropic 有对应取值）。
func TestR17NativeStopNotNotedAnthropic(t *testing.T) {
	_, notes, err := inboundCodec{}.EncodeResponseLossy(&ir.Response{
		ID: "msg_1", Model: "m", StopReason: ir.StopContextWindow})
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if hasNote(notes, foldNote) {
		t.Errorf("本族 context_window 被误报折叠：%v", notes)
	}

	enc := newStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "msg_1", Model: "m"}); err != nil {
		t.Fatalf("Encode(start): %v", err)
	}
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopContextWindow}); err != nil {
		t.Fatalf("Encode(delta): %v", err)
	}
	enc.Finish()
	if hasNote(enc.Notes(), foldNote) {
		t.Errorf("流式本族 context_window 误报：%v", enc.Notes())
	}
}

// 普通档（end_turn）不报：本就无需折叠。
func TestR17OrdinaryStopNotNotedAnthropic(t *testing.T) {
	enc := newStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "msg_1", Model: "m"}); err != nil {
		t.Fatalf("Encode(start): %v", err)
	}
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn}); err != nil {
		t.Fatalf("Encode(delta): %v", err)
	}
	enc.Finish()
	if hasNote(enc.Notes(), foldNote) {
		t.Errorf("end_turn 误报折叠：%v", enc.Notes())
	}
}
