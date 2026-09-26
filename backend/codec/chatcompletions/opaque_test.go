package chatcompletions

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 未知 part 型解码归不透明：判别值、整块原文、来路族都留住。
func TestDecodeUnknownPartToOpaque(t *testing.T) {
	raw := json.RawMessage(`{"type":"input_video","video":{"id":"vid_1"}}`)
	b, err := decodePart(raw)
	if err != nil {
		t.Fatalf("decodePart: %v", err)
	}
	if b.Type != ir.BlockOpaque || b.Opaque == nil {
		t.Fatalf("want opaque block, got %#v", b)
	}
	if b.Opaque.WireType != "input_video" || b.Opaque.From != Name || b.Opaque.Item {
		t.Fatalf("opaque meta = %#v", b.Opaque)
	}
	if string(b.Opaque.Body) != string(raw) {
		t.Fatalf("body not verbatim:\n got %s\nwant %s", b.Opaque.Body, raw)
	}
}

// part 逐字段解失败但带 type 时仍归不透明，而不是连累整个 content 数组。
func TestDecodePartShapeConflictToOpaque(t *testing.T) {
	// wirePart.InputAudio 是对象；给字符串会让逐字段解失败，但 type 读得出。
	raw := json.RawMessage(`{"type":"input_audio","input_audio":"not-an-object"}`)
	b, err := decodePart(raw)
	if err != nil {
		t.Fatalf("decodePart: %v", err)
	}
	if b.Type != ir.BlockOpaque || b.Opaque == nil || b.Opaque.WireType != "input_audio" {
		t.Fatalf("want opaque input_audio, got %#v", b)
	}
}

// 同族编码逐字回吐成 part；跨族报错。
func TestEncodeContentOpaque(t *testing.T) {
	body := json.RawMessage(`{"type":"input_video","video":{"id":"vid_1"}}`)

	// 同族：与一个文本块并存，逼出 parts 数组形态（全文本会收敛成字符串）。
	same, err := encodeContent([]ir.Block{
		{Type: ir.BlockText, Text: "hi"},
		{Type: ir.BlockOpaque, Opaque: &ir.Opaque{WireType: "input_video", Body: body, From: Name}},
	})
	if err != nil {
		t.Fatalf("encodeContent same-family: %v", err)
	}
	if !strings.Contains(string(same), `"input_video"`) || !strings.Contains(string(same), `"vid_1"`) {
		t.Fatalf("verbatim part missing: %s", same)
	}

	// 跨族：报错，不静默丢弃也不降级。
	_, err = encodeContent([]ir.Block{
		{Type: ir.BlockOpaque, Opaque: &ir.Opaque{WireType: "input_video", Body: body, From: codec.ProtocolAnthropic}},
	})
	if err == nil {
		t.Fatal("want an error carrying a foreign opaque part into chat_completions")
	}
	if !strings.Contains(err.Error(), "opaque") {
		t.Fatalf("error should mention opaque: %v", err)
	}
}

// 请求侧同族往返：客户端发来的未知 part 经 DecodeRequest→EncodeRequest 逐字回到线上。
func TestRequestRoundTripUnknownPart(t *testing.T) {
	part := `{"type":"input_video","video":{"id":"vid_1"}}`
	reqBody := `{"model":"gpt-x","messages":[{"role":"user","content":[{"type":"text","text":"look"},` + part + `]}]}`
	req, err := DecodeRequest([]byte(reqBody))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	wire, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(wire), `"input_video"`) || !strings.Contains(string(wire), `"vid_1"`) {
		t.Fatalf("verbatim part missing from re-encoded request: %s", wire)
	}
}

// 流式响应编码器遇到不透明块报错而非静默落进 openBlock：本协议的流式增量
// 是字符串，装不下结构化 part；且响应侧不透明块只可能跨族来。
func TestStreamEncoderRejectsOpaque(t *testing.T) {
	e := newStreamEncoder()
	_, err := e.Encode(ir.Event{
		Type:  ir.EvBlockStart,
		Index: 0,
		Block: &ir.Block{Type: ir.BlockOpaque,
			Opaque: &ir.Opaque{WireType: "code_execution_tool_result", Body: json.RawMessage(`{}`), From: codec.ProtocolAnthropic}},
	})
	if err == nil {
		t.Fatal("want an error streaming a foreign opaque part")
	}
	if !strings.Contains(err.Error(), "opaque") {
		t.Fatalf("error should mention opaque: %v", err)
	}
}
