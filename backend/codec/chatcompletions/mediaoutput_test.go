package chatcompletions

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 模型产出的附件块在流式里整块跳过，损耗经 Notes() 报出。
func TestStreamEncoderDropsMediaBlock(t *testing.T) {
	enc := newStreamEncoder()
	feed := func(ev ir.Event) [][]byte {
		out, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("encode %v: %v", ev.Type, err)
		}
		return out
	}
	var frames [][]byte
	frames = append(frames, feed(ir.Event{Type: ir.EvMessageStart, MessageID: "c1", Model: "m"})...)
	frames = append(frames, feed(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
		Type: ir.BlockImage, Media: &ir.Media{MediaType: "image/png", Data: "AA=="}}})...)
	frames = append(frames, feed(ir.Event{Type: ir.EvBlockStop, Index: 0})...)
	frames = append(frames, feed(ir.Event{Type: ir.EvMessageStop})...)

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

// 非流式：附件块不得编成用户侧才有的 image_url part（非法的助手消息形状），
// 跳过且注记报出。
func TestEncodeResponseLossyDropsMedia(t *testing.T) {
	resp := &ir.Response{
		ID: "c1", Model: "m",
		Content: []ir.Block{
			{Type: ir.BlockText, Text: "look"},
			{Type: ir.BlockImage, Media: &ir.Media{MediaType: "image/png", Data: "AA=="}},
		},
	}
	body, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if strings.Contains(string(body), "image_url") {
		t.Fatalf("附件被编成 image_url part: %s", body)
	}
	found := false
	for _, n := range notes {
		if strings.Contains(n, "1 image(s)") {
			found = true
		}
	}
	if !found {
		t.Fatalf("缺附件丢弃注记: %v", notes)
	}
}
