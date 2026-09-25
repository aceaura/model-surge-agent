package chatcompletions

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// image_url 的裸字符串形态也要解得出来：客户端确实混发（new-api/sub2api
// 都对两路做分支），只认对象会让图片连同所在消息一起消失。
func TestDecodeImageURLBareString(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":"https://example.com/a.png"}]}]}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(req.Messages) != 1 || len(req.Messages[0].Content) != 1 {
		t.Fatalf("消息或部件丢了: %+v", req.Messages)
	}
	b := req.Messages[0].Content[0]
	if b.Type != ir.BlockImage || b.Media == nil || b.Media.URL != "https://example.com/a.png" {
		t.Fatalf("图片没解出来: %+v", b)
	}
}

// 空壳图片（无 base64/URL/file_id）不得编出 {"url":""} 的非法形状：
// 部件跳过；若它是消息里唯一的部件，整条落约定占位而不是凭空消失。
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
	if strings.Contains(s, `"url":""`) || strings.Contains(s, "data:;base64,") {
		t.Fatalf("空壳图片编出了非法形状: %s", s)
	}
	if !strings.Contains(s, codec.ConversationPlaceholder) {
		t.Fatalf("消息被部件拖空后没落占位: %s", s)
	}
}

// 只带 file_id 的 Responses 图片投到 chat：图片槽位不认 file_id，跳过；
// 损耗要有注记。
func TestFileIDOnlyImageDroppedWithNote(t *testing.T) {
	req := &ir.Request{
		Model: "m",
		Messages: []ir.Message{{
			Role: ir.RoleUser,
			Content: []ir.Block{
				{Type: ir.BlockText, Text: "what is this"},
				{Type: ir.BlockImage, Media: &ir.Media{FileID: "file-abc"}},
			},
		}},
	}
	body, notes, err := outboundCodec{}.EncodeRequestLossy(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if strings.Contains(string(body), "file-abc") {
		t.Fatalf("file_id 被当 URL 透传: %s", body)
	}
	found := false
	for _, n := range notes {
		if strings.Contains(n, "file reference") {
			found = true
		}
	}
	if !found {
		t.Fatalf("缺 file_id 丢弃注记: %v", notes)
	}
}

// file 部件的 file_id 同族往返：解码进 Media.FileID，编码原样带回。
func TestFileIDRoundTrip(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[` +
		`{"type":"file","file":{"filename":"spec.pdf","file_id":"file-xyz"}}]}]}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	b := req.Messages[0].Content[0]
	if b.Media == nil || b.Media.FileID != "file-xyz" || b.Media.URL != "" {
		t.Fatalf("file_id 没进 FileID 槽位: %+v", b.Media)
	}
	out, err := outboundCodec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(string(out), `"file_id":"file-xyz"`) {
		t.Fatalf("file_id 没原样带回: %s", out)
	}
}
