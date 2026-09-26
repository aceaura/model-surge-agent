package responses

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 未知 part 型解码归不透明（Item=false：消息内的 part）。
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

// 未知条目类型整块归不透明（Item=true：独立条目），归进助手回合。
func TestDecodeUnknownItemToOpaque(t *testing.T) {
	reqBody := `{"model":"gpt-x","input":[{"type":"file_search_call","id":"fs_1","status":"completed","queries":["a"]}]}`
	req, err := DecodeRequest([]byte(reqBody))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	var found *ir.Opaque
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockOpaque {
				found = b.Opaque
			}
		}
	}
	if found == nil {
		t.Fatalf("no opaque item captured: %#v", req.Messages)
	}
	if found.WireType != "file_search_call" || found.From != Name || !found.Item {
		t.Fatalf("opaque meta = %#v", found)
	}
	if !strings.Contains(string(found.Body), `"queries"`) {
		t.Fatalf("body lost unmodeled keys: %s", found.Body)
	}
}

// 同族编码：条目级不透明块回吐成独立 item，part 级并进消息 content。
func TestEncodeMessageOpaqueSameFamily(t *testing.T) {
	itemBody := json.RawMessage(`{"type":"file_search_call","id":"fs_1","queries":["a"]}`)
	partBody := json.RawMessage(`{"type":"input_video","video":{"id":"vid_1"}}`)
	items, err := encodeMessage(ir.Message{
		Role: ir.RoleAssistant,
		Content: []ir.Block{
			{Type: ir.BlockText, Text: "hi"},
			{Type: ir.BlockOpaque, Opaque: &ir.Opaque{WireType: "input_video", Body: partBody, From: Name}},
			{Type: ir.BlockOpaque, Opaque: &ir.Opaque{WireType: "file_search_call", Body: itemBody, From: Name, Item: true}},
		},
	})
	if err != nil {
		t.Fatalf("encodeMessage: %v", err)
	}
	raw, err := json.Marshal(items)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// 条目级不透明块作为独立 item 逐字回吐。
	if !strings.Contains(string(raw), `"file_search_call"`) || !strings.Contains(string(raw), `"queries"`) {
		t.Fatalf("standalone opaque item missing: %s", raw)
	}
	// part 级不透明块并进消息 content 逐字回吐。
	if !strings.Contains(string(raw), `"input_video"`) || !strings.Contains(string(raw), `"vid_1"`) {
		t.Fatalf("opaque part missing: %s", raw)
	}
}

// 跨族编码报错（请求侧）。
func TestEncodeMessageOpaqueCrossFamilyErrors(t *testing.T) {
	body := json.RawMessage(`{"type":"input_video","video":{"id":"vid_1"}}`)
	_, err := encodeMessage(ir.Message{
		Role: ir.RoleUser,
		Content: []ir.Block{
			{Type: ir.BlockOpaque, Opaque: &ir.Opaque{WireType: "input_video", Body: body, From: codec.ProtocolAnthropic}},
		},
	})
	if err == nil {
		t.Fatal("want an error carrying a foreign opaque part into responses")
	}
	if !strings.Contains(err.Error(), "opaque") {
		t.Fatalf("error should mention opaque: %v", err)
	}
}

// 请求侧同族往返：未知条目经 DecodeRequest→EncodeRequest 逐字回到线上。
func TestRequestRoundTripUnknownItem(t *testing.T) {
	reqBody := `{"model":"gpt-x","input":[{"type":"file_search_call","id":"fs_1","queries":["a"]}]}`
	req, err := DecodeRequest([]byte(reqBody))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	wire, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(wire), `"file_search_call"`) || !strings.Contains(string(wire), `"queries"`) {
		t.Fatalf("verbatim item missing from re-encoded request: %s", wire)
	}
}

// 响应侧不透明块只可能跨族来（本族响应解码器把未知托管条目走计数+注记，
// 不产 opaque），非流式响应编码器报错而非静默丢弃。
func TestResponseEncoderRejectsOpaque(t *testing.T) {
	_, _, err := inboundCodec{}.EncodeResponseLossy(&ir.Response{
		Content: []ir.Block{
			{Type: ir.BlockOpaque, Opaque: &ir.Opaque{
				WireType: "code_execution_tool_result", Body: json.RawMessage(`{}`), From: codec.ProtocolAnthropic,
			}},
		},
	})
	if err == nil {
		t.Fatal("want an error encoding a foreign opaque block into a responses output")
	}
	if !strings.Contains(err.Error(), "opaque") {
		t.Fatalf("error should mention opaque: %v", err)
	}
}

// 流式响应编码器同样报错，而非落进 openBlock 编成凭空的空 message 条目。
func TestStreamEncoderRejectsOpaque(t *testing.T) {
	e := newStreamEncoder()
	_, err := e.Encode(ir.Event{
		Type:  ir.EvBlockStart,
		Index: 0,
		Block: &ir.Block{Type: ir.BlockOpaque,
			Opaque: &ir.Opaque{WireType: "code_execution_tool_result", Body: json.RawMessage(`{}`), From: codec.ProtocolAnthropic}},
	})
	if err == nil {
		t.Fatal("want an error streaming a foreign opaque block")
	}
	if !strings.Contains(err.Error(), "opaque") {
		t.Fatalf("error should mention opaque: %v", err)
	}
}
