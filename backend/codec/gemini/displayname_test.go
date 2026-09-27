package gemini

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 官方 Blob.displayName 与 FileData.displayName（皆可选，"the name used to refer
// to this blob/file to the model, e.g. my_file.pdf"）承载附件文件名。此前 gemini
// 出站只写 mimeType + data/fileUri，IR Media.Name（由 chat file.filename /
// responses input_file.filename / anthropic document.title 设入）整条丢弃、无注记
// ——文件名对文档类附件是有语义的（模型据此区分多个附件、引用出处）。这组测试
// 钉死内联与 URL 两条投递路径都把 Media.Name 落进 displayName。

func encodeReq(t *testing.T, media *ir.Media) wireRequest {
	t.Helper()
	body, err := EncodeRequest(&ir.Request{
		Model: "gemini-3-pro",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockDocument, Media: media},
		}}},
	})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	var w wireRequest
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return w
}

// 内联字节走 Blob：displayName 带上文件名。
func TestGeminiInlineBlobCarriesDisplayName(t *testing.T) {
	w := encodeReq(t, &ir.Media{MediaType: "application/pdf", Data: "JVBERi0", Name: "annual-report.pdf"})
	blob := w.Contents[0].Parts[0].InlineData
	if blob == nil {
		t.Fatalf("want inlineData blob, got %#v", w.Contents[0].Parts[0])
	}
	if blob.DisplayName != "annual-report.pdf" {
		t.Fatalf("inlineData.displayName = %q, want the filename", blob.DisplayName)
	}
}

// 远程 URL 走 FileData：displayName 带上文件名。
func TestGeminiFileDataCarriesDisplayName(t *testing.T) {
	w := encodeReq(t, &ir.Media{MediaType: "application/pdf", URL: "https://x/y.pdf", Name: "y.pdf"})
	fd := w.Contents[0].Parts[0].FileData
	if fd == nil {
		t.Fatalf("want fileData, got %#v", w.Contents[0].Parts[0])
	}
	if fd.DisplayName != "y.pdf" {
		t.Fatalf("fileData.displayName = %q, want the filename", fd.DisplayName)
	}
	if fd.FileURI != "https://x/y.pdf" {
		t.Fatalf("fileData.fileUri = %q", fd.FileURI)
	}
}

// 无文件名时不写 displayName 键（omitempty），不伪造空标签。
func TestGeminiNoDisplayNameWhenNameEmpty(t *testing.T) {
	body, err := EncodeRequest(&ir.Request{
		Model: "gemini-3-pro",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockDocument, Media: &ir.Media{MediaType: "application/pdf", Data: "JVBERi0"}},
		}}},
	})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if strings.Contains(string(body), "displayName") {
		t.Fatalf("empty Media.Name should not emit displayName: %s", body)
	}
}
