package gemini

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 空壳文档（只有 file_id、无 base64/URL）投到 gemini：本协议没有文件引用这一维，
// 照编是 data:"" 的空 inlineData，上游拒收。应降级为文本占位并报有损注记。
func TestEncodeDowngradesEmptyShellDocument(t *testing.T) {
	req := shapeBaseRequest()
	req.Messages[0].Content = []ir.Block{
		{Type: ir.BlockText, Text: "read this"},
		{Type: ir.BlockDocument, Media: &ir.Media{Name: "x.pdf", FileID: "file-abc"}},
	}
	body, notes, err := (outboundCodec{}).EncodeRequestLossy(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	s := string(body)
	if strings.Contains(s, `"data":""`) {
		t.Fatalf("空壳文档编出了空 inlineData: %s", s)
	}
	if strings.Contains(s, "file-abc") {
		t.Fatalf("file_id 泄进了 gemini 请求体: %s", s)
	}
	// 降级为文本：文件名应出现在占位文本里，让模型知道这里本有个文件。
	if !strings.Contains(s, "x.pdf") {
		t.Fatalf("空壳文档未降级为带文件名的文本占位: %s", s)
	}
	var noted bool
	for _, n := range notes {
		if strings.Contains(n, "no payload") {
			noted = true
		}
	}
	if !noted {
		t.Fatalf("空壳文档缺有损注记: %v", notes)
	}
}
