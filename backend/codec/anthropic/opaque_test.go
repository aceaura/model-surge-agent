package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 未知块型解码归不透明：判别值、整块原文、来路族都留住。
// Anthropic 的服务端工具结果块（code_execution_tool_result 等）没有 text
// 字段，降级成文本会丢内容并打断 server_tool_use/结果块的配平。
func TestDecodeUnknownBlockToOpaque(t *testing.T) {
	raw := json.RawMessage(`{"type":"code_execution_tool_result","tool_use_id":"tu_1","content":"print(1)"}`)
	b, ok, err := decodeRawBlock(raw)
	if err != nil || !ok {
		t.Fatalf("decodeRawBlock: ok=%v err=%v", ok, err)
	}
	if b.Type != ir.BlockOpaque || b.Opaque == nil {
		t.Fatalf("want opaque block, got %#v", b)
	}
	if b.Opaque.WireType != "code_execution_tool_result" || b.Opaque.From != Name {
		t.Fatalf("opaque meta = %#v", b.Opaque)
	}
	if string(b.Opaque.Body) != string(raw) {
		t.Fatalf("body not verbatim:\n got %s\nwant %s", b.Opaque.Body, raw)
	}
}

// 块内字段形状冲突（source 在该块型上是字符串而非对象）时，整块留成不透明
// 而不是让 json.Unmarshal 失败连累兄弟块。
func TestDecodeShapeConflictToOpaque(t *testing.T) {
	// wireBlock.Source 是对象；这里给字符串，逐字段解会失败，但 type 读得出。
	raw := json.RawMessage(`{"type":"mystery","source":"not-an-object"}`)
	b, ok, err := decodeRawBlock(raw)
	if err != nil || !ok {
		t.Fatalf("decodeRawBlock: ok=%v err=%v", ok, err)
	}
	if b.Type != ir.BlockOpaque || b.Opaque == nil || b.Opaque.WireType != "mystery" {
		t.Fatalf("want opaque mystery, got %#v", b)
	}
}

// 连判别值都读不出来才算真畸形：报错，与本仓「输入未知即拒」一致。
func TestDecodeNoTypeErrors(t *testing.T) {
	if _, _, err := decodeRawBlock(json.RawMessage(`{"foo":"bar"}`)); err == nil {
		t.Fatal("want an error for a block with no type")
	}
	if _, _, err := decodeRawBlock(json.RawMessage(`42`)); err == nil {
		t.Fatal("want an error for a non-object element")
	}
}

// 同族编码逐字回吐：wireBlock.Raw 整块吐出，键集与原文一致。
func TestEncodeOpaqueSameFamilyVerbatim(t *testing.T) {
	body := json.RawMessage(`{"type":"code_execution_tool_result","tool_use_id":"tu_1","content":"print(1)"}`)
	out, ok, err := encodeBlock(ir.Block{
		Type:   ir.BlockOpaque,
		Opaque: &ir.Opaque{WireType: "code_execution_tool_result", Body: body, From: Name},
	})
	if err != nil || !ok {
		t.Fatalf("encodeBlock: ok=%v err=%v", ok, err)
	}
	got, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("not verbatim:\n got %s\nwant %s", got, body)
	}
}

// 跨族编码报错：不透明块的判别值只在产出它的那一族里有定义，逐字发过去是
// 目标上游不认识的块型，降级成文本又会污染正文。
func TestEncodeOpaqueCrossFamilyErrors(t *testing.T) {
	body := json.RawMessage(`{"type":"code_execution_tool_result","content":"x"}`)
	_, _, err := encodeBlock(ir.Block{
		Type:   ir.BlockOpaque,
		Opaque: &ir.Opaque{WireType: "code_execution_tool_result", Body: body, From: codec.ProtocolResponses},
	})
	if err == nil {
		t.Fatal("want an error carrying a foreign opaque block into anthropic")
	}
	if !strings.Contains(err.Error(), "opaque") {
		t.Fatalf("error should mention opaque: %v", err)
	}
}

// 请求侧同族往返：客户端发来的未知块经 DecodeRequest→EncodeRequest 逐字回到线上。
func TestRequestRoundTripUnknownBlock(t *testing.T) {
	body := `{"type":"tool_search_tool_result","tool_use_id":"tu_9","content":[{"type":"text","text":"found"}]}`
	reqBody := `{"model":"claude-opus-5","messages":[{"role":"user","content":[` + body + `]}]}`
	req, err := DecodeRequest([]byte(reqBody))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if len(req.Messages) != 1 || len(req.Messages[0].Content) != 1 {
		t.Fatalf("decoded shape = %#v", req.Messages)
	}
	if req.Messages[0].Content[0].Type != ir.BlockOpaque {
		t.Fatalf("want opaque, got %v", req.Messages[0].Content[0].Type)
	}
	wire, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(wire), `"tool_search_tool_result"`) {
		t.Fatalf("verbatim block missing from re-encoded request: %s", wire)
	}
	if !strings.Contains(string(wire), `"found"`) {
		t.Fatalf("inner content lost: %s", wire)
	}
}
