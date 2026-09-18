package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 探针实证的缺口：上游是 responses 时签名来源是 responses，
// 编给 anthropic 客户端必须剥离，否则客户端把它存进历史，
// 下一轮带着别家密文回来会被 anthropic 上游整轮拒收。
func TestStreamEncoderStripsForeignSignature(t *testing.T) {
	enc := inboundCodec{}.NewStreamEncoder()
	encodeEvent(t, enc, ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
		Type: ir.BlockThinking, Thinking: &ir.Thinking{},
	}})
	out := encodeEvent(t, enc, ir.Event{
		Type: ir.EvSigDelta, Index: 0, Text: "sig-from-openai", SignatureFrom: "responses",
	})

	if len(out) != 0 {
		t.Errorf("foreign signature produced frames: %s", out)
	}
	notes := encoderNotes(t, enc)
	if len(notes) != 1 || !strings.Contains(notes[0], "own protocol family") {
		t.Errorf("notes = %#v, want one family note", notes)
	}
}

// 同族签名必须保留：丢掉它会让客户端下一轮回传一个无签名的推理块，
// anthropic 上游同样拒收。
func TestStreamEncoderKeepsSameFamilySignature(t *testing.T) {
	enc := inboundCodec{}.NewStreamEncoder()
	encodeEvent(t, enc, ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
		Type: ir.BlockThinking, Thinking: &ir.Thinking{},
	}})
	out := encodeEvent(t, enc, ir.Event{
		Type: ir.EvSigDelta, Index: 0, Text: "sig-a", SignatureFrom: Name,
	})

	if !strings.Contains(string(joinFrames(out)), "sig-a") {
		t.Errorf("same-family signature was dropped: %s", out)
	}
	if notes := encoderNotes(t, enc); len(notes) != 0 {
		t.Errorf("notes = %#v, want none", notes)
	}
}

// 来源为空按异族处理：签名无从验证时放行的代价是客户端把一段验不了的
// 密文存进历史，下一轮整个请求被拒。
func TestStreamEncoderStripsSignatureWithoutSource(t *testing.T) {
	enc := inboundCodec{}.NewStreamEncoder()
	encodeEvent(t, enc, ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
		Type: ir.BlockThinking, Thinking: &ir.Thinking{},
	}})
	out := encodeEvent(t, enc, ir.Event{Type: ir.EvSigDelta, Index: 0, Text: "sig-unknown"})

	if len(out) != 0 {
		t.Errorf("sourceless signature produced frames: %s", out)
	}
	if notes := encoderNotes(t, enc); len(notes) != 1 {
		t.Errorf("notes = %#v, want one note", notes)
	}
}

// 非流式路径必须与流式给出同样的结论与同样的措辞。
func TestEncodeResponseReportsForeignSignature(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{
		Type: ir.BlockThinking,
		Thinking: &ir.Thinking{
			Text:          "reasoned",
			Signature:     "sig-from-openai",
			SignatureFrom: "responses",
		},
	}}}

	body, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if strings.Contains(string(body), "sig-from-openai") {
		t.Errorf("foreign signature leaked into the response body: %s", body)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "own protocol family") {
		t.Errorf("notes = %#v, want one family note", notes)
	}
}

// 响应体必须与不取诊断时逐字节相同：诊断开关影响客户端可见内容
// 等于让排查动作本身改变被排查的对象。
func TestEncodeResponseLossyIsByteStable(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{
		Type:     ir.BlockThinking,
		Thinking: &ir.Thinking{Text: "t", Signature: "sig-a", SignatureFrom: Name},
	}}}

	plain, err := inboundCodec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	withNotes, _, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatalf("encode lossy: %v", err)
	}
	if string(plain) != string(withNotes) {
		t.Errorf("bytes differ:\n plain = %s\n lossy = %s", plain, withNotes)
	}
}

func encodeEvent(t *testing.T, enc codec.StreamEncoder, ev ir.Event) [][]byte {
	t.Helper()
	out, err := enc.Encode(ev)
	if err != nil {
		t.Fatalf("encode %s: %v", ev.Type, err)
	}
	return out
}

func encoderNotes(t *testing.T, enc codec.StreamEncoder) []string {
	t.Helper()
	n, ok := enc.(codec.StreamNotes)
	if !ok {
		t.Fatal("stream encoder does not report notes")
	}
	return n.Notes()
}

func joinFrames(frames [][]byte) []byte {
	var out []byte
	for _, f := range frames {
		out = append(out, f...)
	}
	return out
}
