package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// document 的 source.type=content（官方 union 第四种形态，正文是块数组）归不透明
// 块：Media 只有 base64 / URL 两种载体，装不下块数组。此前它被当 base64 处理，
// 重编后写出一个连 data 键都没有的 base64 PDF source，正文全丢且形状非法（上游
// 400）。归不透明块后同族逐字往返无损。
func TestDecodeDocumentContentSourceToOpaque(t *testing.T) {
	body := `{"type":"document","source":{"type":"content","content":[{"type":"text","text":"chapter one"}]}}`
	reqBody := `{"model":"claude-opus-5","messages":[{"role":"user","content":[` + body + `]}]}`
	req, err := DecodeRequest([]byte(reqBody))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	b := req.Messages[0].Content[0]
	if b.Type != ir.BlockOpaque || b.Opaque == nil {
		t.Fatalf("want opaque block, got %#v", b)
	}
	if b.Opaque.WireType != "document" || b.Opaque.From != Name {
		t.Fatalf("opaque meta = %#v", b.Opaque)
	}
	// 同族编码逐字回吐：正文与 content 形态都还在。
	wire, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(wire), `"type":"content"`) || !strings.Contains(string(wire), "chapter one") {
		t.Fatalf("content source not carried verbatim: %s", wire)
	}
}

// document 的 context 与 citations.enabled 两项配置落进 Media，同族逐字往返。
func TestDocumentConfigRoundTrip(t *testing.T) {
	body := `{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0"},"context":"annual report","citations":{"enabled":true}}`
	reqBody := `{"model":"claude-opus-5","messages":[{"role":"user","content":[` + body + `]}]}`
	req, err := DecodeRequest([]byte(reqBody))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	m := req.Messages[0].Content[0].Media
	if m == nil {
		t.Fatalf("document did not decode into Media: %#v", req.Messages[0].Content[0])
	}
	if m.Context != "annual report" {
		t.Fatalf("context = %q", m.Context)
	}
	if m.CitationsEnabled == nil || !*m.CitationsEnabled {
		t.Fatalf("citations.enabled = %#v", m.CitationsEnabled)
	}
	wire, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	s := string(wire)
	if !strings.Contains(s, `"context":"annual report"`) {
		t.Fatalf("context lost on re-encode: %s", s)
	}
	if !strings.Contains(s, `"citations":{"enabled":true}`) {
		t.Fatalf("citations config lost on re-encode: %s", s)
	}
}

// citations 三态：键缺失 → nil；显式 {"enabled":false} → 非 nil 的 false。
// 两者语义不同，重编时前者不带 citations 键、后者原样带回 enabled:false。
func TestDocumentCitationsThreeState(t *testing.T) {
	dec := func(body string) *bool {
		reqBody := `{"model":"m","messages":[{"role":"user","content":[` + body + `]}]}`
		req, err := DecodeRequest([]byte(reqBody))
		if err != nil {
			t.Fatalf("DecodeRequest: %v", err)
		}
		return req.Messages[0].Content[0].Media.CitationsEnabled
	}
	base := `{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0"}`
	if got := dec(base + `}`); got != nil {
		t.Fatalf("absent citations should be nil, got %#v", *got)
	}
	if got := dec(base + `,"citations":{"enabled":false}}`); got == nil || *got {
		t.Fatalf("explicit false should be non-nil false, got %#v", got)
	}

	// 显式 false 同族带回；键缺失不带 citations 键。
	enc := func(m *ir.Media) string {
		wire, _, err := encodeBlock(ir.Block{Type: ir.BlockDocument, Media: m})
		if err != nil {
			t.Fatalf("encodeBlock: %v", err)
		}
		b, _ := json.Marshal(wire)
		return string(b)
	}
	f := false
	if s := enc(&ir.Media{MediaType: "application/pdf", Data: "JVBERi0", CitationsEnabled: &f}); !strings.Contains(s, `"citations":{"enabled":false}`) {
		t.Fatalf("explicit false not carried: %s", s)
	}
	if s := enc(&ir.Media{MediaType: "application/pdf", Data: "JVBERi0"}); strings.Contains(s, "citations") {
		t.Fatalf("absent citations should not emit the key: %s", s)
	}
}

// 只有 document 容器写回配置：图片容器即便 Media 上误带了 context 也不该写出，
// 官方 image 块没有 context / citations 键。
func TestConfigOnlyOnDocumentContainer(t *testing.T) {
	f := true
	wire, _, err := encodeBlock(ir.Block{Type: ir.BlockImage, Media: &ir.Media{
		MediaType: "image/png", Data: "iVBOR", Context: "stray", CitationsEnabled: &f,
	}})
	if err != nil {
		t.Fatalf("encodeBlock: %v", err)
	}
	b, _ := json.Marshal(wire)
	if strings.Contains(string(b), "context") || strings.Contains(string(b), "citations") {
		t.Fatalf("image container leaked document config: %s", b)
	}
}
