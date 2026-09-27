package chatcompletions

import (
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次17：跨族终止原因折叠注记的接线（chat 出站）。context_window_exceeded /
// max_messages / steered 三档本协议都无对应值，renderFinishReason 一律塌进
// length，具体成因与补救方向丢失，必须报出。流式与非流式同判据。

const foldNote = "rewrote the stop reason"

func TestR17StopFoldNotedChat(t *testing.T) {
	for _, reason := range []ir.StopReason{ir.StopContextWindow, ir.StopMaxMessages, ir.StopSteered} {
		// 非流式。
		_, notes, err := inboundCodec{}.EncodeResponseLossy(&ir.Response{
			ID: "c1", Model: "m", StopReason: reason})
		if err != nil {
			t.Fatalf("EncodeResponseLossy(%s): %v", reason, err)
		}
		if !anyNoteHas(notes, foldNote) {
			t.Errorf("非流式 %s 折叠未报：%v", reason, notes)
		}
		// 流式。
		enc := newStreamEncoder()
		if _, err := enc.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: reason}); err != nil {
			t.Fatalf("Encode(delta %s): %v", reason, err)
		}
		enc.Finish()
		if !anyNoteHas(enc.Notes(), foldNote) {
			t.Errorf("流式 %s 折叠未报：%v", reason, enc.Notes())
		}
	}
}

// 普通档不报：stop / length 在各协议都有对应值，无需折叠。
func TestR17OrdinaryStopNotNotedChat(t *testing.T) {
	for _, reason := range []ir.StopReason{ir.StopEndTurn, ir.StopMaxTokens, ir.StopToolUse, ir.StopContentFilter} {
		_, notes, err := inboundCodec{}.EncodeResponseLossy(&ir.Response{
			ID: "c1", Model: "m", StopReason: reason})
		if err != nil {
			t.Fatalf("EncodeResponseLossy(%s): %v", reason, err)
		}
		if anyNoteHas(notes, foldNote) {
			t.Errorf("普通档 %s 误报折叠：%v", reason, notes)
		}
	}
}
