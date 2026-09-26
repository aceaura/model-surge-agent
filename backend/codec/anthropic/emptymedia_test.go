package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 空壳文档（只有 file_id、无 base64/URL）不得编出缺 data 的 document source：
// 文件名嗅出 application/pdf，media_type 在而 data 因 omitempty 蒸发，
// {"type":"document","source":{"type":"base64","media_type":"application/pdf"}}
// 缺必填键，上游 400 拒整轮。部件应整块跳过并报有损注记。
func TestEncodeSkipsEmptyShellDocument(t *testing.T) {
	req := &ir.Request{
		Model: "m",
		Messages: []ir.Message{{
			Role: ir.RoleUser,
			Content: []ir.Block{
				{Type: ir.BlockText, Text: "read this"},
				{Type: ir.BlockDocument, Media: &ir.Media{Name: "x.pdf", FileID: "file-abc"}},
			},
		}},
	}
	body, notes, err := outboundCodec{}.EncodeRequestLossy(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	s := string(body)
	if strings.Contains(s, `"type":"document"`) {
		t.Fatalf("空壳文档编出了非法形状: %s", s)
	}
	if strings.Contains(s, "file-abc") {
		t.Fatalf("file_id 泄进了 anthropic 请求体: %s", s)
	}
	// 文本部件仍在，消息没被拖空。
	if !strings.Contains(s, "read this") {
		t.Fatalf("同消息的文本部件被误伤: %s", s)
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

// 空壳音频同样跳过（此前守卫只覆盖图片）。
func TestEncodeSkipsEmptyShellAudio(t *testing.T) {
	req := &ir.Request{
		Model: "m",
		Messages: []ir.Message{{
			Role:    ir.RoleUser,
			Content: []ir.Block{{Type: ir.BlockAudio, Media: &ir.Media{FileID: "audio-1"}}},
		}},
	}
	body, err := outboundCodec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if strings.Contains(string(body), `"type":"image"`) || strings.Contains(string(body), "audio-1") {
		t.Fatalf("空壳音频编出了非法形状或泄了 file_id: %s", body)
	}
}
