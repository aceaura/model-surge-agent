package codec

import (
	"strings"
	"testing"
)

func scanAll(t *testing.T, input string) []Frame {
	t.Helper()
	s := NewFrameScanner(strings.NewReader(input))
	var out []Frame
	for s.Scan() {
		out = append(out, s.Frame())
	}
	if err := s.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return out
}

func TestScanNamedEvents(t *testing.T) {
	got := scanAll(t, "event: message_start\ndata: {\"a\":1}\n\nevent: message_stop\ndata: {}\n\n")
	if len(got) != 2 {
		t.Fatalf("frames = %#v", got)
	}
	if got[0].Event != "message_start" || got[0].Data != `{"a":1}` {
		t.Errorf("frame 0 = %#v", got[0])
	}
	if got[1].Event != "message_stop" {
		t.Errorf("frame 1 = %#v", got[1])
	}
}

// Gemini 的 alt=sse 与部分 Chat Completions 实现只发 data 行。
func TestScanUnnamedEvents(t *testing.T) {
	got := scanAll(t, "data: {\"a\":1}\n\ndata: [DONE]\n\n")
	if len(got) != 2 {
		t.Fatalf("frames = %#v", got)
	}
	if got[0].Event != "" || got[0].Data != `{"a":1}` {
		t.Errorf("frame 0 = %#v", got[0])
	}
	if got[1].Data != "[DONE]" {
		t.Errorf("frame 1 = %#v", got[1])
	}
}

func TestMultipleDataLinesJoinWithNewline(t *testing.T) {
	got := scanAll(t, "data: line1\ndata: line2\n\n")
	if len(got) != 1 || got[0].Data != "line1\nline2" {
		t.Errorf("frames = %#v", got)
	}
}

func TestCRLFAndBlankLineNoise(t *testing.T) {
	got := scanAll(t, "\r\n\r\nevent: ping\r\ndata: {}\r\n\r\n\r\ndata: tail\r\n\r\n")
	if len(got) != 2 {
		t.Fatalf("frames = %#v", got)
	}
	if got[0].Event != "ping" || got[0].Data != "{}" {
		t.Errorf("frame 0 = %#v", got[0])
	}
	if got[1].Data != "tail" {
		t.Errorf("frame 1 = %#v", got[1])
	}
}

func TestCommentLinesAreSkipped(t *testing.T) {
	got := scanAll(t, ": keep-alive\ndata: {}\n\n")
	if len(got) != 1 || got[0].Data != "{}" {
		t.Errorf("frames = %#v", got)
	}
}

// 只去掉冒号后的第一个空格，多余空格属于数据。
func TestOnlyOneLeadingSpaceIsStripped(t *testing.T) {
	got := scanAll(t, "data:  two-spaces\n\n")
	if len(got) != 1 || got[0].Data != " two-spaces" {
		t.Errorf("frames = %#v", got)
	}
}

func TestFieldWithoutColonStartsAFrame(t *testing.T) {
	got := scanAll(t, "data\n\n")
	if len(got) != 1 || got[0].Data != "" {
		t.Errorf("frames = %#v", got)
	}
}

// 上游在半帧处断连时，已读到的字段必须交出去：
// 那可能正是携带 usage 或 stop_reason 的终止帧。
func TestTruncatedFinalFrameIsStillEmitted(t *testing.T) {
	got := scanAll(t, "data: {\"a\":1}\n\ndata: {\"b\":2}")
	if len(got) != 2 {
		t.Fatalf("frames = %#v", got)
	}
	if got[1].Data != `{"b":2}` {
		t.Errorf("frame 1 = %#v", got[1])
	}
}

func TestEmptyStreamYieldsNoFrames(t *testing.T) {
	if got := scanAll(t, ""); len(got) != 0 {
		t.Errorf("frames = %#v", got)
	}
}

func TestUnknownFieldsDoNotBreakFraming(t *testing.T) {
	got := scanAll(t, "id: 42\nretry: 1000\nevent: tick\ndata: {}\n\n")
	if len(got) != 1 || got[0].Event != "tick" || got[0].Data != "{}" {
		t.Errorf("frames = %#v", got)
	}
}

func TestEncodeFrameRoundTrips(t *testing.T) {
	raw := EncodeFrame("message_delta", []byte(`{"a":1}`))
	if string(raw) != "event: message_delta\ndata: {\"a\":1}\n\n" {
		t.Fatalf("encoded = %q", raw)
	}
	got := scanAll(t, string(raw))
	if len(got) != 1 || got[0].Event != "message_delta" || got[0].Data != `{"a":1}` {
		t.Errorf("frames = %#v", got)
	}
}

func TestEncodeFrameWithoutEventName(t *testing.T) {
	raw := EncodeFrame("", []byte("[DONE]"))
	if string(raw) != "data: [DONE]\n\n" {
		t.Fatalf("encoded = %q", raw)
	}
}

// 数据内含换行必须拆成多个 data 行，否则客户端会把换行当帧边界。
func TestEncodeFrameSplitsEmbeddedNewlines(t *testing.T) {
	raw := EncodeFrame("x", []byte("a\nb"))
	if string(raw) != "event: x\ndata: a\ndata: b\n\n" {
		t.Fatalf("encoded = %q", raw)
	}
	got := scanAll(t, string(raw))
	if len(got) != 1 || got[0].Data != "a\nb" {
		t.Errorf("round trip lost the newline: %#v", got)
	}
}
