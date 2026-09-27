package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 官方 document 块的 title（DocumentBlock.title / DocumentBlockParam.title，
// "The title of the document"）是 anthropic 唯一能承载附件文件名的槽位。此前
// wireBlock 未建模它：①解码真实 anthropic 请求里带 title 的 document 时，
// 文件名被 json.Unmarshal 静默吞掉，IR Media.Name 恒空；②异族（chat 的
// file.filename / responses 的 input_file.filename）投影来的文档带了文件名，
// 编码回 anthropic 时整条丢弃、无注记。这组测试把 title 钉死为 Media.Name 的
// anthropic 载体，同族逐字往返、跨族投影也保全。

// 解码：document.title 落进 Media.Name。
func TestDocumentTitleDecodedIntoMediaName(t *testing.T) {
	body := `{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0"},"title":"annual-report.pdf"}`
	reqBody := `{"model":"claude-opus-5","messages":[{"role":"user","content":[` + body + `]}]}`
	req, err := DecodeRequest([]byte(reqBody))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	m := req.Messages[0].Content[0].Media
	if m == nil {
		t.Fatalf("document did not decode into Media: %#v", req.Messages[0].Content[0])
	}
	if m.Name != "annual-report.pdf" {
		t.Fatalf("Media.Name = %q, want the document title", m.Name)
	}
}

// 同族往返：解码再编码，title 逐字带回。
func TestDocumentTitleRoundTrip(t *testing.T) {
	body := `{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0"},"title":"q3.pdf"}`
	reqBody := `{"model":"m","messages":[{"role":"user","content":[` + body + `]}]}`
	req, err := DecodeRequest([]byte(reqBody))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	wire, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(wire), `"title":"q3.pdf"`) {
		t.Fatalf("title lost on re-encode: %s", wire)
	}
}

// 编码：Media.Name 写进 document 容器的 title 槽（跨族投影来的文件名得以保全）。
func TestDocumentTitleEncodedFromMediaName(t *testing.T) {
	wire, _, err := encodeBlock(ir.Block{Type: ir.BlockDocument, Media: &ir.Media{
		MediaType: "application/pdf", Data: "JVBERi0", Name: "invoice.pdf",
	}})
	if err != nil {
		t.Fatalf("encodeBlock: %v", err)
	}
	b, _ := json.Marshal(wire)
	if !strings.Contains(string(b), `"title":"invoice.pdf"`) {
		t.Fatalf("Media.Name not written into title: %s", b)
	}
}

// 只有 document 容器写 title：图片容器即便 Media 上误带了 Name 也不该写出，
// 官方 image 块没有 title 键（与 context/citations 同理，见 TestConfigOnlyOnDocumentContainer）。
func TestTitleOnlyOnDocumentContainer(t *testing.T) {
	wire, _, err := encodeBlock(ir.Block{Type: ir.BlockImage, Media: &ir.Media{
		MediaType: "image/png", Data: "iVBOR", Name: "stray.png",
	}})
	if err != nil {
		t.Fatalf("encodeBlock: %v", err)
	}
	b, _ := json.Marshal(wire)
	if strings.Contains(string(b), "title") {
		t.Fatalf("image container leaked document title: %s", b)
	}
}

// 无文件名的 document 不写 title 键（omitempty），不伪造空标题。
func TestNoTitleWhenMediaNameEmpty(t *testing.T) {
	wire, _, err := encodeBlock(ir.Block{Type: ir.BlockDocument, Media: &ir.Media{
		MediaType: "application/pdf", Data: "JVBERi0",
	}})
	if err != nil {
		t.Fatalf("encodeBlock: %v", err)
	}
	b, _ := json.Marshal(wire)
	if strings.Contains(string(b), "title") {
		t.Fatalf("empty Media.Name should not emit title: %s", b)
	}
}
