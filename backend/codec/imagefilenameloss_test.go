package codec_test

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	_ "github.com/aceaura/model-surge-agent/backend/codec/anthropic"
	_ "github.com/aceaura/model-surge-agent/backend/codec/chatcompletions"
	_ "github.com/aceaura/model-surge-agent/backend/codec/gemini"
	_ "github.com/aceaura/model-surge-agent/backend/codec/responses"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次29（候选 F）：带载荷的图片块携带 Media.Name（来自 chat 的 file part /
// responses 的 input_file，mime 嗅成图片时归 BlockImage 且带名）时，chat 的
// image_url / responses 的 input_image / anthropic 的 image source 都没有文件名
// 字段→静默丢弃。gemini 的 Blob.displayName 接得住。此前只有 gemini 保全、其余
// 三家既不保全也不报，是一处真静默丢弃。这组测试钉住：三家报、gemini 不报、
// file_id-only 图片不报（chat/responses 用 file/input_file part 保住了名）、
// 无文件名不报。

func imageBlockWithName() ir.Block {
	return ir.Block{Type: ir.BlockImage, Media: &ir.Media{
		MediaType: "image/png", Data: "iVBORw0", Name: "photo.png",
	}}
}

func notesFor(t *testing.T, name string, req *ir.Request) string {
	t.Helper()
	oc, ok := codec.Outbound(name)
	if !ok {
		t.Fatalf("outbound %q not registered", name)
	}
	return strings.Join(codec.DescribeLossy(req, name, oc.Caps()), "; ")
}

func TestImageFilenameDroppedOffGemini(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{imageBlockWithName()}},
	}}
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolAnthropic} {
		if got := notesFor(t, name, req); !strings.Contains(got, "image filename") {
			t.Errorf("%s 应报图片文件名丢失：%q", name, got)
		}
	}
	if got := notesFor(t, codec.ProtocolGemini, req); strings.Contains(got, "image filename") {
		t.Errorf("gemini 的 displayName 接得住文件名，误报：%q", got)
	}
}

// file_id-only 的图片不经图片槽位：chat/responses 用 file/input_file part 把
// 文件名保住了，报了就是谎报。这里断言三家都不出「image filename」这一条
// （anthropic 另有「image file reference」维，不在此断言范围）。
func TestNoImageFilenameNoteForFileIDOnly(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockImage, Media: &ir.Media{
			MediaType: "image/png", FileID: "file_abc", Name: "photo.png",
		}}}},
	}}
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolGemini} {
		if got := notesFor(t, name, req); strings.Contains(got, "image filename") {
			t.Errorf("%s 对 file_id-only 图片误报文件名丢失（file part 已保住名）：%q", name, got)
		}
	}
}

// 没有文件名的普通图片不该凭空多出注记。
func TestNoImageFilenameNoteWhenNameAbsent(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockImage, Media: &ir.Media{
			MediaType: "image/png", Data: "iVBORw0",
		}}}},
	}}
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolAnthropic, codec.ProtocolGemini} {
		if got := notesFor(t, name, req); strings.Contains(got, "image filename") {
			t.Errorf("%s 对无文件名图片误报：%q", name, got)
		}
	}
}

// URL-only 图片同样带载荷（HasPayload 为真），文件名一样丢：注记也要报。
func TestImageFilenameDroppedForURLPayload(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockImage, Media: &ir.Media{
			MediaType: "image/png", URL: "https://x/photo.png", Name: "photo.png",
		}}}},
	}}
	if got := notesFor(t, codec.ProtocolChatCompletions, req); !strings.Contains(got, "image filename") {
		t.Errorf("URL-only 图片也应报文件名丢失：%q", got)
	}
}
