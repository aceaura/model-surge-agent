package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 非流式：上游产出的音频（本协议白名单只含图片 + PDF）被降级为文本占位，
// 必须由 EncodeResponseLossy 报出——与 chat/responses 两族的 MediaOutputDropNote
// 对称，否则客户端只看到一段占位文本、不知道这里本来有个附件。
func TestResponseLossyReportsDowngradedAudio(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{
		Type:  ir.BlockAudio,
		Media: &ir.Media{MediaType: "audio/wav", Data: "AAAA"},
	}}}
	_, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !hasNoteSubstr(notes, "from the model output") {
		t.Errorf("缺媒体降级注记：%#v", notes)
	}
}

// 非流式：图片与 PDF 本协议装得下，不得误报降级。
func TestResponseLossyKeepsImageAndPDF(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{
		{Type: ir.BlockImage, Media: &ir.Media{MediaType: "image/png", Data: "AAAA"}},
		{Type: ir.BlockDocument, Media: &ir.Media{MediaType: "application/pdf", Data: "AAAA"}},
	}}
	_, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatal(err)
	}
	if hasNoteSubstr(notes, "from the model output") {
		t.Errorf("图片/PDF 被误报为降级：%#v", notes)
	}
}

// 非流式：空壳媒体（无载荷）走整块跳过路径、不是降级，不计入媒体降级注记。
func TestResponseLossyEmptyShellNotCountedAsDowngrade(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{
		Type:  ir.BlockAudio,
		Media: &ir.Media{MediaType: "audio/wav"}, // 无 Data 无 URL
	}}}
	_, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatal(err)
	}
	if hasNoteSubstr(notes, "from the model output") {
		t.Errorf("空壳媒体被误报为降级：%#v", notes)
	}
}

// 流式：同一降级经 Notes() 报出，判据与非流式同源。
func TestStreamReportsDowngradedMedia(t *testing.T) {
	enc := inboundCodec{}.NewStreamEncoder(nil)
	encodeEvent(t, enc, ir.Event{Type: ir.EvBlockStart, Index: 0,
		Block: &ir.Block{Type: ir.BlockAudio, Media: &ir.Media{MediaType: "audio/mpeg", Data: "AAAA"}}})

	notes := encoderNotes(t, enc)
	if !hasNoteSubstr(notes, "from the model output") {
		t.Errorf("流式缺媒体降级注记：%#v", notes)
	}
}

// 流式：图片块本协议装得下，不得误报。
func TestStreamKeepsImage(t *testing.T) {
	enc := inboundCodec{}.NewStreamEncoder(nil)
	encodeEvent(t, enc, ir.Event{Type: ir.EvBlockStart, Index: 0,
		Block: &ir.Block{Type: ir.BlockImage, Media: &ir.Media{MediaType: "image/png", Data: "AAAA"}}})

	if notes := encoderNotes(t, enc); hasNoteSubstr(notes, "from the model output") {
		t.Errorf("流式图片被误报为降级：%#v", notes)
	}
}

// 计数分类：音频归「非图片附件」，与 MediaOutputDropNote 的措辞对应。
func TestResponseDowngradedMediaWording(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{
		Type:  ir.BlockFile,
		Media: &ir.Media{MediaType: "text/csv", Data: "AAAA", Name: "x.csv"},
	}}}
	_, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(notes, "; ")
	if !strings.Contains(joined, "non-image attachment") {
		t.Errorf("非图片附件措辞缺失：%q", joined)
	}
}
