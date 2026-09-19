package pipeline

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// failingEncoder 对指定事件类型返回错误，其余照常编。
type failingEncoder struct {
	failOn ir.EventType
}

func (e *failingEncoder) Encode(ev ir.Event) ([][]byte, error) {
	if ev.Type == e.failOn {
		return nil, errors.New("cannot express this event")
	}
	return [][]byte{[]byte("frame\n")}, nil
}

func (e *failingEncoder) Finish() [][]byte { return nil }

// 跳过事件必须出说明：客户端收到的内容缺了一块，而流正常收束，
// 不留说明的话这件事在诊断里完全不存在。
func TestSkippedEventProducesNote(t *testing.T) {
	var rec Record
	w := httptest.NewRecorder()
	enc := &failingEncoder{failOn: ir.EvTextDelta}

	err := writeEvents(w, enc, ir.Event{Type: ir.EvTextDelta, Text: "hi"}, nil, &rec)
	if err != nil {
		t.Fatalf("跳过事件不该中断流：%v", err)
	}
	notes := rec.mergedLossy()
	if len(notes) != 1 {
		t.Fatalf("说明 = %d 条，want 1：%v", len(notes), notes)
	}
	// 带事件类型：跳掉一段文本与跳掉一次工具调用的后果差得远。
	if !strings.Contains(notes[0], string(ir.EvTextDelta)) {
		t.Errorf("说明里没有事件类型：%q", notes[0])
	}
}

// 不同事件类型给出不同说明：合成一条会让两种后果分不开。
func TestSkippedEventNoteNamesTheEventType(t *testing.T) {
	cases := []ir.EventType{ir.EvTextDelta, ir.EvToolInput, ir.EvThinkingDelta}
	for _, typ := range cases {
		t.Run(string(typ), func(t *testing.T) {
			var rec Record
			enc := &failingEncoder{failOn: typ}
			_ = writeEvents(httptest.NewRecorder(), enc, ir.Event{Type: typ}, nil, &rec)

			notes := rec.mergedLossy()
			if len(notes) != 1 {
				t.Fatalf("说明 = %d 条：%v", len(notes), notes)
			}
			if !strings.Contains(notes[0], string(typ)) {
				t.Errorf("说明 %q 里没有 %q", notes[0], typ)
			}
		})
	}
}

// 「跳过事件保住流」的策略不变：其余事件照常写出。
func TestSkippedEventDoesNotStopTheStream(t *testing.T) {
	var rec Record
	w := httptest.NewRecorder()
	enc := &failingEncoder{failOn: ir.EvThinkingDelta}

	if err := writeEvents(w, enc, ir.Event{Type: ir.EvThinkingDelta}, nil, &rec); err != nil {
		t.Fatalf("跳过事件不该中断流：%v", err)
	}
	if err := writeEvents(w, enc, ir.Event{Type: ir.EvTextDelta, Text: "hi"}, nil, &rec); err != nil {
		t.Fatalf("后续事件写失败：%v", err)
	}
	if got := w.Body.String(); got != "frame\n" {
		t.Errorf("写出的字节 = %q，want 只有后一个事件那一帧", got)
	}
}

// 未发生跳过时不产说明：无条件产说明会让每条流水都带一条噪音。
func TestNoSkipNoNote(t *testing.T) {
	var rec Record
	enc := &failingEncoder{failOn: ir.EvToolInput}
	if err := writeEvents(httptest.NewRecorder(), enc,
		ir.Event{Type: ir.EvTextDelta, Text: "hi"}, nil, &rec); err != nil {
		t.Fatalf("写失败：%v", err)
	}
	if notes := rec.mergedLossy(); len(notes) != 0 {
		t.Errorf("没跳过却产了说明：%v", notes)
	}
}

// 同类跳过多次只报一条：mergedLossy 去重，说明文本因此不能含随机成分。
func TestRepeatedSkipsOfSameTypeCollapse(t *testing.T) {
	var rec Record
	enc := &failingEncoder{failOn: ir.EvTextDelta}
	for range 3 {
		_ = writeEvents(httptest.NewRecorder(), enc,
			ir.Event{Type: ir.EvTextDelta, Text: "x"}, nil, &rec)
	}
	if notes := rec.mergedLossy(); len(notes) != 1 {
		t.Errorf("说明 = %d 条，want 1（同类应去重）：%v", len(notes), notes)
	}
}
