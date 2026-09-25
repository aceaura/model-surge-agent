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

// 只带 file_id 的 Responses 图片投到 anthropic：本族没有文件引用这一维，
// 跳过；detail 档位同样没有槽位，两维损耗都要有注记。
func TestForeignImageLossesNoted(t *testing.T) {
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
	if strings.Contains(string(body), "file-abc") {
		t.Fatalf("file_id 泄进了 anthropic 请求体: %s", body)
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
	if !refNote {
		t.Fatalf("缺 file_id 丢弃注记: %v", notes)
	}
	if !detailNote {
		t.Fatalf("缺 detail 丢弃注记: %v", notes)
	}
}
