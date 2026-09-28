package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次48：anthropic 官方 image_block_param 与 document_block_param 的 source union
// 都含 {type:"file",file_id}（FileImageSourceParam / FileDocumentSourceParam，据
// anthropic-sdk-python 核实）。此前 wireSource 只建模 type/media_type/data/url，file
// 源的 file_id 被 json.Unmarshal 静默吞掉 → 媒体变空壳 → 同族 anthropic→anthropic
// 往返时编码器按 !HasPayload 整块跳过，整个图片/文档块蒸发。这组测试钉住：file 源
// 解码入 IR（含按 wire 容器还原图片/文档块型）、同族逐字往返、IR 直构编码、base64 源
// 不受影响、FileID 熬过 Clone。

const imgFileBody = `{"type":"image","source":{"type":"file","file_id":"file_img_1"}}`

const docFileBody = `{"type":"document","source":{"type":"file","file_id":"file_doc_1"},"title":"report.pdf"}`

func TestImageFileSourceDecodedIntoIR(t *testing.T) {
	reqBody := `{"model":"claude-opus-5","messages":[{"role":"user","content":[` + imgFileBody + `]}]}`
	req, err := DecodeRequest([]byte(reqBody))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	blk := req.Messages[0].Content[0]
	if blk.Type != ir.BlockImage {
		t.Fatalf("file 源的图片块型应按 wire 容器还原为 BlockImage，得到 %v", blk.Type)
	}
	if blk.Media == nil || blk.Media.FileID != "file_img_1" {
		t.Fatalf("file_id 没读进 IR.Media：%#v", blk.Media)
	}
}

func TestDocumentFileSourceDecodedIntoIR(t *testing.T) {
	reqBody := `{"model":"claude-opus-5","messages":[{"role":"user","content":[` + docFileBody + `]}]}`
	req, err := DecodeRequest([]byte(reqBody))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	blk := req.Messages[0].Content[0]
	if blk.Type != ir.BlockDocument {
		t.Fatalf("file 源的文档块型应按 wire 容器还原为 BlockDocument，得到 %v", blk.Type)
	}
	if blk.Media == nil || blk.Media.FileID != "file_doc_1" {
		t.Fatalf("file_id 没读进 IR.Media：%#v", blk.Media)
	}
	if blk.Media.Name != "report.pdf" {
		t.Errorf("document.title 应落进 Media.Name，得到 %q", blk.Media.Name)
	}
}

func TestImageFileSourceRoundTripsVerbatim(t *testing.T) {
	reqBody := `{"model":"claude-opus-5","messages":[{"role":"user","content":[` + imgFileBody + `]}]}`
	req, err := DecodeRequest([]byte(reqBody))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	wire, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	s := string(wire)
	if !strings.Contains(s, `"type":"file"`) || !strings.Contains(s, `"file_id":"file_img_1"`) {
		t.Fatalf("图片 file 源同族往返丢失：%s", s)
	}
	// 还原成 image 容器，不能误写成 document。
	if !strings.Contains(s, `"type":"image"`) {
		t.Errorf("图片 file 源应还原为 image 容器：%s", s)
	}
}

func TestDocumentFileSourceRoundTripsVerbatim(t *testing.T) {
	reqBody := `{"model":"claude-opus-5","messages":[{"role":"user","content":[` + docFileBody + `]}]}`
	req, err := DecodeRequest([]byte(reqBody))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	wire, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	s := string(wire)
	if !strings.Contains(s, `"type":"file"`) || !strings.Contains(s, `"file_id":"file_doc_1"`) {
		t.Fatalf("文档 file 源同族往返丢失：%s", s)
	}
	if !strings.Contains(s, `"type":"document"`) {
		t.Errorf("文档 file 源应还原为 document 容器：%s", s)
	}
	if !strings.Contains(s, `"title":"report.pdf"`) {
		t.Errorf("文档 file 源往返应带回 title：%s", s)
	}
}

// IR 直构路径（不经解码器）：图片只带 FileID、无 Data/URL，encodeBlock 应写出 file 源
// 而非整块跳过。
func TestEncodeBlockWritesImageFileSourceFromIR(t *testing.T) {
	wb, ok, err := encodeBlock(ir.Block{Type: ir.BlockImage, Media: &ir.Media{FileID: "file_x"}})
	if err != nil || !ok {
		t.Fatalf("encodeBlock: ok=%v err=%v", ok, err)
	}
	b, _ := json.Marshal(wb)
	if !strings.Contains(string(b), `"type":"file"`) || !strings.Contains(string(b), `"file_id":"file_x"`) {
		t.Fatalf("图片 file 源没写出：%s", b)
	}
}

// 音频块只带 FileID：anthropic 没有音频 file 源，encodeBlock 应跳过（ok=false），
// 不凭空造一个非法形状。
func TestEncodeBlockSkipsAudioFileSource(t *testing.T) {
	_, ok, err := encodeBlock(ir.Block{Type: ir.BlockAudio, Media: &ir.Media{FileID: "file_a"}})
	if err != nil {
		t.Fatalf("encodeBlock: %v", err)
	}
	if ok {
		t.Fatalf("anthropic 无音频 file 源，音频 file_id 块应被跳过而非编出")
	}
}

// base64 源不受影响：不带 file_id 时不写 file 源、不写 file_id 键。
func TestBase64SourceUnaffectedByFileID(t *testing.T) {
	body := `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}`
	reqBody := `{"model":"claude-opus-5","messages":[{"role":"user","content":[` + body + `]}]}`
	req, err := DecodeRequest([]byte(reqBody))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if req.Messages[0].Content[0].Media.FileID != "" {
		t.Fatalf("base64 源不该有 FileID：%#v", req.Messages[0].Content[0].Media)
	}
	wire, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if strings.Contains(string(wire), "file_id") || strings.Contains(string(wire), `"type":"file"`) {
		t.Fatalf("base64 源被误写成 file 源：%s", wire)
	}
}

// FileID 必须熬过 Clone：请求侧编码会 Clone 整份请求，字符串字段随 cloneBlocks 的
// `v := *b.Media` 值拷贝自动带上，此测钉住不漏。
func TestFileIDSurvivesClone(t *testing.T) {
	req := &ir.Request{Model: "m", Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockImage, Media: &ir.Media{
			FileID: "file_clone",
		}}}},
	}}
	cl := req.Clone()
	m := cl.Messages[0].Content[0].Media
	if m == nil || m.FileID != "file_clone" {
		t.Fatalf("FileID 没熬过 Clone：%#v", m)
	}
}
