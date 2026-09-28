package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 只带 file_id 的文档（无 base64/URL）：anthropic 官方 document_block 的 source
// union 含 FileDocumentSourceParam（{type:"file",file_id}），故应编出**合法的 file
// 源**逐字投递，而不是早先误以为的「缺 data 的非法 base64 源」→ 整块跳过。轮次48
// 据 anthropic-sdk-python 核实修正：本族原生收文件引用，同族往返无损、不报有损注记。
// （真正三载体全空、连 file_id 都没有的空壳文档仍走跳过分支，见 TestEncodeSkipsEmptyShellAudio
// 的同型逻辑与 imagefidelity 的空壳图片用例。）
func TestEncodeEmitsFileSourceForFileIDOnlyDocument(t *testing.T) {
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
	if !strings.Contains(s, `"type":"document"`) || !strings.Contains(s, `"type":"file"`) ||
		!strings.Contains(s, `"file_id":"file-abc"`) {
		t.Fatalf("file_id-only 文档没编出合法 file 源: %s", s)
	}
	// 不得编出缺 data 的非法 base64 源。
	if strings.Contains(s, `"type":"base64"`) {
		t.Fatalf("编出了非法 base64 源: %s", s)
	}
	// 文件名随 document.title 带回。
	if !strings.Contains(s, `"title":"x.pdf"`) {
		t.Errorf("文档文件名没随 title 带回: %s", s)
	}
	// 文本部件仍在，消息没被拖空。
	if !strings.Contains(s, "read this") {
		t.Fatalf("同消息的文本部件被误伤: %s", s)
	}
	// 同族可投递，不该报「无可投递载荷」。
	for _, n := range notes {
		if strings.Contains(n, "no payload") {
			t.Fatalf("file 源可投递，不该报有损注记: %v", notes)
		}
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
