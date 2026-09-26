package chatcompletions

import "testing"

// delta.content 数组里的非文本 part（图片/音频/文件）在流式解码时无处安放：
// 中立的流式表示只把文本增量转发给客户端。跳过本身没错，错在此前静默——
// decodeDelta 里一句 `b.Type != ir.BlockText` 直接 continue，没有计数。
// responses 的 droppedMsgParts、gemini 的 droppedUnknownParts 都计数报出，
// 本协议唯独漏了，这里钉住补齐后的行为（判据与那两族同口径）。

const nonTextPartNote = "content part(s) of a kind this conversion does not map"

func TestStreamDropsNonTextContentPartsWithNote(t *testing.T) {
	dec := newStreamDecoder()
	frames := []string{
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}}]}`,
		doneSentinel,
	}
	for _, f := range frames {
		if _, err := dec.Feed("", f); err != nil {
			t.Fatalf("feed %s: %v", f, err)
		}
	}
	if !anyNoteHas(dec.Notes(), nonTextPartNote) {
		t.Errorf("非文本 content part 丢弃没被报出：%v", dec.Notes())
	}
}

// 多个非文本 part 累加成一条注记、报总数：逐 part 一条会让去重挡不住、
// 客户端看到一堆同文案不同数字的说明。
func TestStreamCountsMultipleNonTextParts(t *testing.T) {
	dec := newStreamDecoder()
	frames := []string{
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}},{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}]}}]}`,
		doneSentinel,
	}
	for _, f := range frames {
		if _, err := dec.Feed("", f); err != nil {
			t.Fatalf("feed %s: %v", f, err)
		}
	}
	if !anyNoteHas(dec.Notes(), "dropped 2 message content part(s)") {
		t.Errorf("两个非文本 part 未合并计数：%v", dec.Notes())
	}
}

// 纯文本增量不触发该注记：文本正是本变换转发的东西，报出来是假警报。
func TestStreamTextOnlyNoContentPartNote(t *testing.T) {
	dec := newStreamDecoder()
	frames := []string{
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		doneSentinel,
	}
	for _, f := range frames {
		if _, err := dec.Feed("", f); err != nil {
			t.Fatalf("feed %s: %v", f, err)
		}
	}
	if anyNoteHas(dec.Notes(), nonTextPartNote) {
		t.Errorf("纯文本被误报为丢弃 part：%v", dec.Notes())
	}
}

// 空文本 part 不算丢失：本就没内容可转发，跳过但不计数。钉住「非文本才计数」
// 这条判据——把空文本也算进丢弃数会虚报，让客户端以为有附件被吞了。
func TestStreamEmptyTextPartNotCountedAsDropped(t *testing.T) {
	dec := newStreamDecoder()
	frames := []string{
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":[{"type":"text","text":""}]}}]}`,
		doneSentinel,
	}
	for _, f := range frames {
		if _, err := dec.Feed("", f); err != nil {
			t.Fatalf("feed %s: %v", f, err)
		}
	}
	if anyNoteHas(dec.Notes(), nonTextPartNote) {
		t.Errorf("空文本 part 被误计为丢弃：%v", dec.Notes())
	}
}
