package chatcompletions

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 零增量工具调用关块时补一个 "{}" 增量：客户端只从 delta 拼参数，
// 一个增量都不发就拼出 ""，json.loads 直接崩。
func TestStreamEncoderZeroDeltaToolGetsEmptyObject(t *testing.T) {
	enc := newStreamEncoder()
	feed := func(ev ir.Event) [][]byte {
		out, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("encode %v: %v", ev.Type, err)
		}
		return out
	}
	var frames [][]byte
	frames = append(frames, feed(ir.Event{Type: ir.EvMessageStart, MessageID: "m1", Model: "m"})...)
	frames = append(frames, feed(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
		Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "c1", Name: "f"}}})...)
	frames = append(frames, feed(ir.Event{Type: ir.EvBlockStop, Index: 0})...)
	frames = append(frames, feed(ir.Event{Type: ir.EvMessageStop})...)

	s := joinChunkFrames(frames)
	if strings.Count(s, `"arguments":"{}"`) != 1 {
		t.Fatalf("零增量工具调用必须恰好补一次 \"{}\":\n%s", s)
	}
}

// 对照组：有真实增量的调用不补 "{}"。
func TestStreamEncoderToolWithDeltaNotPatched(t *testing.T) {
	enc := newStreamEncoder()
	feed := func(ev ir.Event) [][]byte {
		out, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("encode %v: %v", ev.Type, err)
		}
		return out
	}
	var frames [][]byte
	frames = append(frames, feed(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
		Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "c1", Name: "f"}}})...)
	frames = append(frames, feed(ir.Event{Type: ir.EvToolInput, Index: 0, Text: `{"a":1}`})...)
	frames = append(frames, feed(ir.Event{Type: ir.EvBlockStop, Index: 0})...)
	frames = append(frames, feed(ir.Event{Type: ir.EvMessageStop})...)

	s := joinChunkFrames(frames)
	if strings.Contains(s, `"arguments":"{}"`) {
		t.Fatalf("有增量的调用不应补 \"{}\":\n%s", s)
	}
}

// 流被掐断（没有 EvBlockStop）时 Finish 也要补。
func TestStreamEncoderZeroDeltaToolPatchedOnFinish(t *testing.T) {
	enc := newStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
		Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "c1", Name: "f"}}}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	s := joinChunkFrames(enc.Finish())
	if strings.Count(s, `"arguments":"{}"`) != 1 {
		t.Fatalf("Finish 没补零增量工具调用的 \"{}\":\n%s", s)
	}
}

func joinChunkFrames(frames [][]byte) string {
	var sb strings.Builder
	for _, f := range frames {
		sb.Write(f)
	}
	return sb.String()
}
