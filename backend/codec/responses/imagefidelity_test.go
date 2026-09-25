package responses

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// input_image 的 image_url 也要接受 chat 风格的对象形态，嵌套的 detail
// 一并收下（顶层 detail 优先）。
func TestDecodeInputImageObjectForm(t *testing.T) {
	body := []byte(`{"model":"m","input":[{"type":"message","role":"user","content":[` +
		`{"type":"input_image","image_url":{"url":"https://example.com/a.png","detail":"low"}}]}]}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(req.Messages) != 1 || len(req.Messages[0].Content) != 1 {
		t.Fatalf("消息或部件丢了: %+v", req.Messages)
	}
	b := req.Messages[0].Content[0]
	if b.Type != ir.BlockImage || b.Media == nil ||
		b.Media.URL != "https://example.com/a.png" || b.Media.Detail != "low" {
		t.Fatalf("图片没解出来: %+v", b)
	}
}

// 只给 file_id 的 input_image 不再是 400：file_id 是本族图片槽位的第二种
// 合法载体，解码进 Media.FileID，编码原样带回。
func TestInputImageFileIDRoundTrip(t *testing.T) {
	body := []byte(`{"model":"m","input":[{"type":"message","role":"user","content":[` +
		`{"type":"input_image","file_id":"file-img-1","detail":"high"}]}]}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	b := req.Messages[0].Content[0]
	if b.Media == nil || b.Media.FileID != "file-img-1" || b.Media.Detail != "high" {
		t.Fatalf("file_id 图片没解出来: %+v", b.Media)
	}
	out, err := outboundCodec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"file_id":"file-img-1"`) || !strings.Contains(s, `"detail":"high"`) {
		t.Fatalf("file_id/detail 没原样带回: %s", s)
	}
}

// 空壳图片（三载体全空）不得编出连 image_url 键都没有的 input_image；
// 若它是消息里唯一的部件，整条落约定占位而不是从 input 里消失。
func TestEncodeSkipsEmptyShellImage(t *testing.T) {
	req := &ir.Request{
		Model: "m",
		Messages: []ir.Message{{
			Role: ir.RoleUser,
			Content: []ir.Block{
				{Type: ir.BlockImage, Media: &ir.Media{}},
			},
		}},
	}
	body, err := outboundCodec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	s := string(body)
	if strings.Contains(s, "input_image") {
		t.Fatalf("空壳图片编出了非法形状: %s", s)
	}
	if !strings.Contains(s, codec.ConversationPlaceholder) {
		t.Fatalf("消息被部件拖空后没落占位: %s", s)
	}
}

// input_file 的 file_id 同族往返。
func TestInputFileIDRoundTrip(t *testing.T) {
	body := []byte(`{"model":"m","input":[{"type":"message","role":"user","content":[` +
		`{"type":"input_file","filename":"spec.pdf","file_id":"file-doc-1"}]}]}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	b := req.Messages[0].Content[0]
	if b.Media == nil || b.Media.FileID != "file-doc-1" || b.Media.URL != "" {
		t.Fatalf("file_id 没进 FileID 槽位: %+v", b.Media)
	}
	out, err := outboundCodec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(string(out), `"file_id":"file-doc-1"`) {
		t.Fatalf("file_id 没原样带回: %s", out)
	}
}
