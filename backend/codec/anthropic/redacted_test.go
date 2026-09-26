package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// D2：涂抹块（redacted_thinking）在同族流式路径上逐字往返。上游把密文随
// content_block_start 全量下发（涂抹块没有 delta），解码器要把它连同密文一起
// 交给 IR；同族编码器再原样吐回，客户端下一轮才还原得出这段被涂抹的推理。
func TestStreamDecodePreservesRedactedThinking(t *testing.T) {
	dec := newStreamDecoder()
	got, err := dec.Feed(evContentBlockStart,
		`{"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"opaque"}}`)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(got) != 1 || got[0].Type != ir.EvBlockStart {
		t.Fatalf("want a single block_start, got %+v", got)
	}
	b := got[0].Block
	if b == nil || b.Type != ir.BlockThinking || b.Thinking == nil {
		t.Fatalf("block_start carries no thinking block: %#v", b)
	}
	if !b.Thinking.Redacted || b.Thinking.RedactedData != "opaque" {
		t.Errorf("ciphertext must survive decode: %#v", b.Thinking)
	}
}

func TestStreamEncodeEmitsRedactedThinkingVerbatim(t *testing.T) {
	enc := newStreamEncoder()
	frames, err := enc.Encode(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
		Type:     ir.BlockThinking,
		Thinking: &ir.Thinking{Redacted: true, RedactedData: "opaque", SignatureFrom: Name},
	}})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	out := join(frames)
	if !strings.Contains(out, `"type":"redacted_thinking"`) {
		t.Errorf("same-family encoder must emit redacted_thinking, got %s", out)
	}
	if !strings.Contains(out, `"data":"opaque"`) {
		t.Errorf("ciphertext must be replayed verbatim, got %s", out)
	}
}

// 端到端：上游涂抹帧经解码→再编码，密文一字不差地回到线上。
func TestStreamRedactedThinkingRoundTrips(t *testing.T) {
	dec := newStreamDecoder()
	evs, err := dec.Feed(evContentBlockStart,
		`{"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"Zm9vYmFy"}}`)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	enc := newStreamEncoder()
	var out string
	for _, ev := range evs {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		out += join(frames)
	}
	if !strings.Contains(out, `"data":"Zm9vYmFy"`) {
		t.Errorf("round trip lost the ciphertext: %s", out)
	}
}
