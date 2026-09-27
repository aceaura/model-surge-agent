package chatcompletions

import (
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次18：跨族「命中的停止序列回显值」丢弃注记的接线（chat 出站）。本协议 chunk /
// 响应体只有 finish_reason，无字段回显被命中的那条序列原文；chat 客户端可发 ≤4 条
// stop，配 anthropic 上游时命中值真实可触达却被静默丢弃。流式与非流式同判据。

const stopSeqNote = "dropped the matched stop sequence"

func TestR18StopSequenceNotedChat(t *testing.T) {
	// 非流式。
	_, notes, err := inboundCodec{}.EncodeResponseLossy(&ir.Response{
		ID: "c1", Model: "m", StopReason: ir.StopStopSequence, StopSequence: "STOP"})
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if !anyNoteHas(notes, stopSeqNote) {
		t.Errorf("非流式命中序列未报：%v", notes)
	}

	// 流式：命中序列随 EvMessageDelta 抵达（真流式与整份响应投影都落这里）。
	enc := newStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageDelta,
		StopReason: ir.StopStopSequence, StopSequence: "STOP"}); err != nil {
		t.Fatalf("Encode(delta): %v", err)
	}
	enc.Finish()
	if !anyNoteHas(enc.Notes(), stopSeqNote) {
		t.Errorf("流式命中序列未报：%v", enc.Notes())
	}
}

// 原因不是 stop_sequence，或上游没给序列原文：无值可丢，两路径都不报（防假阳性）。
func TestR18StopSequenceNotNotedChat(t *testing.T) {
	cases := []struct {
		reason ir.StopReason
		seq    string
	}{
		{ir.StopEndTurn, "STOP"},   // 原因非 stop_sequence，编码侧不采纳序列
		{ir.StopMaxTokens, "STOP"}, // 同上
		{ir.StopStopSequence, ""},  // 原因是 stop_sequence 但上游没给原文
	}
	for _, c := range cases {
		_, notes, err := inboundCodec{}.EncodeResponseLossy(&ir.Response{
			ID: "c1", Model: "m", StopReason: c.reason, StopSequence: c.seq})
		if err != nil {
			t.Fatalf("EncodeResponseLossy(%s,%q): %v", c.reason, c.seq, err)
		}
		if anyNoteHas(notes, stopSeqNote) {
			t.Errorf("非流式 %s(seq=%q) 误报：%v", c.reason, c.seq, notes)
		}

		enc := newStreamEncoder()
		if _, err := enc.Encode(ir.Event{Type: ir.EvMessageDelta,
			StopReason: c.reason, StopSequence: c.seq}); err != nil {
			t.Fatalf("Encode(delta %s,%q): %v", c.reason, c.seq, err)
		}
		enc.Finish()
		if anyNoteHas(enc.Notes(), stopSeqNote) {
			t.Errorf("流式 %s(seq=%q) 误报：%v", c.reason, c.seq, enc.Notes())
		}
	}
}

// 注记只报一次：Notes() 被多次调用不重复堆积（DedupeNotes + 判据稳定）。
func TestR18StopSequenceNoteDrainedOnce(t *testing.T) {
	enc := newStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageDelta,
		StopReason: ir.StopStopSequence, StopSequence: "STOP"}); err != nil {
		t.Fatalf("Encode(delta): %v", err)
	}
	enc.Finish()
	n := 0
	for _, s := range enc.Notes() {
		if anyNoteHas([]string{s}, stopSeqNote) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("命中序列注记应恰一条，实得 %d：%v", n, enc.Notes())
	}
}
