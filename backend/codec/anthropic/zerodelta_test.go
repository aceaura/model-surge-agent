package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 零增量工具块关块时补一个 "{}" delta：客户端只从 input_json_delta
// 拼参数，一个 delta 都不发就拼出空串而非合法 JSON。
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
		Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "t1", Name: "f"}}})...)
	frames = append(frames, feed(ir.Event{Type: ir.EvBlockStop, Index: 0})...)
	frames = append(frames, feed(ir.Event{Type: ir.EvMessageStop})...)

	s := string(joinFrames(frames))
	if !strings.Contains(s, `"input_json_delta"`) || !strings.Contains(s, `"partial_json":"{}"`) {
		t.Fatalf("零增量工具块没补 \"{}\" delta:\n%s", s)
	}
	if strings.Count(s, `"partial_json":"{}"`) != 1 {
		t.Fatalf("\"{}\" 必须恰好补一次:\n%s", s)
	}
	// 补的 delta 必须在 content_block_stop 之前。
	if strings.Index(s, `"partial_json":"{}"`) > strings.Index(s, `"content_block_stop"`) {
		t.Fatalf("补充的 delta 落在块闭合之后:\n%s", s)
	}
}

// 对照组：有真实增量的工具块不补 "{}"。
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
		Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "t1", Name: "f"}}})...)
	frames = append(frames, feed(ir.Event{Type: ir.EvToolInput, Index: 0, Text: `{"a":1}`})...)
	frames = append(frames, feed(ir.Event{Type: ir.EvBlockStop, Index: 0})...)
	frames = append(frames, feed(ir.Event{Type: ir.EvMessageStop})...)

	s := string(joinFrames(frames))
	if strings.Contains(s, `"partial_json":"{}"`) {
		t.Fatalf("有增量的工具块不应补 \"{}\":\n%s", s)
	}
}

// 流被掐断（没有 EvBlockStop）时 Finish 也要补。
func TestStreamEncoderZeroDeltaToolPatchedOnFinish(t *testing.T) {
	enc := newStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
		Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "t1", Name: "f"}}}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	s := string(joinFrames(enc.Finish()))
	if !strings.Contains(s, `"partial_json":"{}"`) {
		t.Fatalf("Finish 没补零增量工具块的 \"{}\":\n%s", s)
	}
}
