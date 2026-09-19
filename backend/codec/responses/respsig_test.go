package responses

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 探针实证的缺口：anthropic 上游的 signature 编给 responses 客户端时
// 会被塞进 encrypted_content，而 OpenAI 侧只能解自己的密文，
// 客户端回传时整轮被拒。
func TestStreamEncoderDoesNotAccumulateForeignSignature(t *testing.T) {
	enc := inboundCodec{}.NewStreamEncoder(nil)
	encodeEvent(t, enc, ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
		Type: ir.BlockThinking, Thinking: &ir.Thinking{},
	}})
	encodeEvent(t, enc, ir.Event{
		Type: ir.EvSigDelta, Index: 0, Text: "sig-a", SignatureFrom: "anthropic",
	})
	frames := append(encodeEvent(t, enc, ir.Event{Type: ir.EvBlockStop, Index: 0}),
		enc.Finish()...)

	if strings.Contains(string(joinFrames(frames)), "sig-a") {
		t.Errorf("foreign signature leaked into the stream: %s", joinFrames(frames))
	}
	notes := encoderNotes(t, enc)
	if len(notes) != 1 || !strings.Contains(notes[0], "own protocol family") {
		t.Errorf("notes = %#v, want one family note", notes)
	}
}

// 同族签名必须攒进 encrypted_content：丢掉它会让客户端下一轮回传
// 一个无密文的 reasoning 条目，上游同样拒收。
func TestStreamEncoderKeepsSameFamilySignature(t *testing.T) {
	enc := inboundCodec{}.NewStreamEncoder(nil)
	encodeEvent(t, enc, ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
		Type: ir.BlockThinking, Thinking: &ir.Thinking{},
	}})
	encodeEvent(t, enc, ir.Event{
		Type: ir.EvSigDelta, Index: 0, Text: "gAAAA-own", SignatureFrom: Name,
	})
	frames := append(encodeEvent(t, enc, ir.Event{Type: ir.EvBlockStop, Index: 0}),
		enc.Finish()...)

	if !strings.Contains(string(joinFrames(frames)), "gAAAA-own") {
		t.Errorf("same-family signature was dropped: %s", joinFrames(frames))
	}
	if notes := encoderNotes(t, enc); len(notes) != 0 {
		t.Errorf("notes = %#v, want none", notes)
	}
}

func TestEncodeResponseReportsForeignSignature(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{
		Type: ir.BlockThinking,
		Thinking: &ir.Thinking{
			Text:          "reasoned",
			Signature:     "sig-a",
			SignatureFrom: "anthropic",
		},
	}}}

	body, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if strings.Contains(string(body), "sig-a") {
		t.Errorf("foreign signature leaked into the response body: %s", body)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "own protocol family") {
		t.Errorf("notes = %#v, want one family note", notes)
	}
}

func TestEncodeResponseLossyIsByteStable(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{
		Type:     ir.BlockThinking,
		Thinking: &ir.Thinking{Text: "t", Signature: "gAAAA-own", SignatureFrom: Name},
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
