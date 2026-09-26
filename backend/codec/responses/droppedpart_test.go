package responses

import (
	"strings"
	"testing"
)

// 非文本/拒绝的 message content part（如音频输出）此前在流式路径被静默丢弃，
// 而非流式路径会留成 opaque 块。流式必须计数并报出，客户端才看得到丢了个 part。
func TestStreamDropsUnknownMessagePartWithNote(t *testing.T) {
	d := newStreamDecoder()
	if _, err := d.Feed("", `{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"audio"}}`); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	d.Finish()
	var found bool
	for _, n := range d.Notes() {
		if strings.Contains(n, "content part") {
			found = true
		}
	}
	if !found {
		t.Errorf("未知 message part 应报注记，实得 %v", d.Notes())
	}
}

// 文本 part 照常开块，不该被误计入丢弃。
func TestStreamTextPartNotCountedAsDropped(t *testing.T) {
	d := newStreamDecoder()
	if _, err := d.Feed("", `{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	d.Finish()
	for _, n := range d.Notes() {
		if strings.Contains(n, "content part") {
			t.Errorf("文本 part 不该被计为丢弃：%v", n)
		}
	}
}
