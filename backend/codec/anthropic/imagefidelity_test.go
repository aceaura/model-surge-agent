package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 空壳图片不得编出缺 media_type 与 data 的 base64 source（上游 400 拒整轮）：
// 部件跳过；若它是消息里唯一的部件，整条落约定占位而不是空 content 数组。
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
	if strings.Contains(s, `"type":"image"`) {
		t.Fatalf("空壳图片编出了非法形状: %s", s)
	}
	if !strings.Contains(s, codec.ConversationPlaceholder) {
		t.Fatalf("消息被部件拖空后没落占位: %s", s)
	}
}

// 只带 file_id 的 Responses 图片投到 anthropic：轮次48 据 anthropic-sdk-python 核实，
// 官方 image_block 的 source union 含 FileImageSourceParam（{type:"file",file_id}），
// 本族**有**图片文件引用这一维（caps.ImageFileRef 真）→ file_id 逐字投递、不报「图片
// 引用丢失」。但 detail 档位仍无槽位（anthropic 图片源不带 detail，caps.ImageDetail 假），
// 这一维照样丢弃并报注记。
func TestForeignImageFileRefDeliveredDetailStillNoted(t *testing.T) {
	req := &ir.Request{
		Model: "m",
		Messages: []ir.Message{{
			Role: ir.RoleUser,
			Content: []ir.Block{
				{Type: ir.BlockText, Text: "what is this"},
				{Type: ir.BlockImage, Media: &ir.Media{FileID: "file-abc", Detail: "high"}},
			},
		}},
	}
	body, notes, err := outboundCodec{}.EncodeRequestLossy(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	s := string(body)
	// file_id 现在合法投递为 image file 源。
	if !strings.Contains(s, `"type":"image"`) || !strings.Contains(s, `"type":"file"`) ||
		!strings.Contains(s, `"file_id":"file-abc"`) {
		t.Fatalf("图片 file_id 没编出合法 file 源: %s", s)
	}
	var refNote, detailNote bool
	for _, n := range notes {
		if strings.Contains(n, "file reference") {
			refNote = true
		}
		if strings.Contains(n, "detail") {
			detailNote = true
		}
	}
	if refNote {
		t.Fatalf("anthropic 原生收图片 file 引用，不该报 file reference 丢弃: %v", notes)
	}
	if !detailNote {
		t.Fatalf("缺 detail 丢弃注记: %v", notes)
	}
}
