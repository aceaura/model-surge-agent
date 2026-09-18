package chatcompletions

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 本协议没有签名字段，来源无论是谁都丢。行为本身早已如此，
// 这里钉住的是「丢弃要有说明」：客户端看不到签名时得知道是协议限制，
// 而不是上游没给。
func TestStreamEncoderReportsUnsupportedSignature(t *testing.T) {
	for _, from := range []string{"anthropic", "responses", Name, ""} {
		enc := inboundCodec{}.NewStreamEncoder()
		out := encodeEvent(t, enc, ir.Event{
			Type: ir.EvSigDelta, Index: 0, Text: "sig", SignatureFrom: from,
		})
		if len(out) != 0 {
			t.Errorf("from=%q produced frames: %s", from, out)
		}
		notes := encoderNotes(t, enc)
		if len(notes) != 1 || !strings.Contains(notes[0], "no signed reasoning") {
			t.Errorf("from=%q notes = %#v, want one unsupported note", from, notes)
		}
	}
}

// 没有签名就没有说明：无损的流水不该多出一条空洞的诊断。
func TestStreamEncoderSilentWithoutSignature(t *testing.T) {
	enc := inboundCodec{}.NewStreamEncoder()
	encodeEvent(t, enc, ir.Event{Type: ir.EvThinkingDelta, Index: 0, Text: "reasoned"})
	if notes := encoderNotes(t, enc); len(notes) != 0 {
		t.Errorf("notes = %#v, want none", notes)
	}
}

func TestEncodeResponseReportsUnsupportedSignature(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{
		Type:     ir.BlockThinking,
		Thinking: &ir.Thinking{Text: "t", Signature: "sig", SignatureFrom: "anthropic"},
	}}}

	body, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if strings.Contains(string(body), `"sig"`) {
		t.Errorf("signature leaked into the response body: %s", body)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "no signed reasoning") {
		t.Errorf("notes = %#v, want one unsupported note", notes)
	}
}

func TestEncodeResponseLossyIsByteStable(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{
		Type:     ir.BlockThinking,
		Thinking: &ir.Thinking{Text: "t", Signature: "sig", SignatureFrom: "anthropic"},
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
