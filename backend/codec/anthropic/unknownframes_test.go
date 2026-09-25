package anthropic

import (
	"strings"
	"testing"
)

// 未知事件型/delta 型计数注记：上游新增帧型此前被静默丢弃，客户端无从
// 知道有载荷蒸发。现在计入 droppedUnknown，经 Notes() 一次性报出。

func TestUnknownStreamFramesCounted(t *testing.T) {
	dec := newStreamDecoder()
	for _, frame := range [][2]string{
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"frobnicate_delta","x":1}}`},
		{"frobnicate_event", `{"type":"frobnicate_event","data":{}}`},
	} {
		if _, err := dec.Feed(frame[0], frame[1]); err != nil {
			t.Fatalf("Feed(%s): %v", frame[0], err)
		}
	}
	notes := dec.Notes()
	if len(notes) != 1 || !strings.Contains(notes[0], "2 stream event(s) or delta(s)") {
		t.Fatalf("未知帧计数注记不对：%q", notes)
	}
	// 计数器读后排干：同一条流反复取 Notes 不该重复报账。
	if again := dec.Notes(); len(again) != 0 {
		t.Errorf("Notes() 未排干：%q", again)
	}
}

// 对照组：已知事件型走完一轮零注记（原生形态协议必须闭嘴）。
func TestKnownStreamFramesLeaveNoNotes(t *testing.T) {
	dec := newStreamDecoder()
	for _, frame := range [][2]string{
		{"message_start", `{"type":"message_start","message":{"id":"msg_1","model":"claude","usage":{"input_tokens":1}}}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`},
		{"message_stop", `{"type":"message_stop"}`},
	} {
		if _, err := dec.Feed(frame[0], frame[1]); err != nil {
			t.Fatalf("Feed(%s): %v", frame[0], err)
		}
	}
	dec.Finish()
	if notes := dec.Notes(); len(notes) != 0 {
		t.Errorf("已知帧全程不该有注记：%q", notes)
	}
}
