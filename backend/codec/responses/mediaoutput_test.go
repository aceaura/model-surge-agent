package responses

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 模型产出的附件块不得伪造成空 message 条目：整块跳过、不占 output_index，
// 损耗经 Notes() 报出。
func TestStreamEncoderDropsMediaBlockWithoutBurningIndex(t *testing.T) {
	enc := newStreamEncoder()
	feed := func(ev ir.Event) [][]byte {
		out, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("encode %v: %v", ev.Type, err)
		}
		return out
	}
	var frames [][]byte
	frames = append(frames, feed(ir.Event{Type: ir.EvMessageStart, MessageID: "r1", Model: "m"})...)
	frames = append(frames, feed(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
		Type: ir.BlockImage, Media: &ir.Media{MediaType: "image/png", Data: "AA=="}}})...)
	frames = append(frames, feed(ir.Event{Type: ir.EvBlockStop, Index: 0})...)
	// 后续真块的 output_index 必须是 0：图片块不许烧掉一个序号。
	frames = append(frames, feed(ir.Event{Type: ir.EvBlockStart, Index: 1, Block: &ir.Block{Type: ir.BlockText}})...)
	frames = append(frames, feed(ir.Event{Type: ir.EvTextDelta, Index: 1, Text: "hi"})...)
	frames = append(frames, feed(ir.Event{Type: ir.EvBlockStop, Index: 1})...)
	frames = append(frames, feed(ir.Event{Type: ir.EvMessageStop})...)

	var sb strings.Builder
	for _, f := range frames {
		sb.Write(f)
	}
	s := sb.String()
	// 只允许一个 message 条目（文本那条），且它落在 output_index 0。
	if strings.Count(s, `"type":"response.output_item.added"`) != 1 {
		t.Fatalf("媒体块伪造了条目:\n%s", s)
	}
	if !strings.Contains(s, `"output_index":0`) {
		t.Fatalf("文本块没拿到 output_index 0（序号被烧）:\n%s", s)
	}

	notes := enc.Notes()
	found := false
	for _, n := range notes {
		if strings.Contains(n, "1 image(s)") {
			found = true
		}
	}
	if !found {
		t.Fatalf("Notes 缺附件丢弃说明: %v", notes)
	}
}

// 非流式：附件块跳过且注记报出，正文不受影响。
func TestEncodeResponseLossyDropsMedia(t *testing.T) {
	resp := &ir.Response{
		ID: "r1", Model: "m",
		Content: []ir.Block{
			{Type: ir.BlockText, Text: "look"},
			{Type: ir.BlockImage, Media: &ir.Media{MediaType: "image/png", Data: "AA=="}},
			{Type: ir.BlockDocument, Media: &ir.Media{MediaType: "application/pdf", Data: "BB=="}},
		},
	}
	body, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	s := string(body)
	if strings.Contains(s, "input_image") || strings.Contains(s, `"file"`) {
		t.Fatalf("附件泄进响应体: %s", s)
	}
	found := false
	for _, n := range notes {
		if strings.Contains(n, "1 image(s)") && strings.Contains(n, "1 non-image attachment(s)") {
			found = true
		}
	}
	if !found {
		t.Fatalf("缺附件丢弃注记: %v", notes)
	}
}
