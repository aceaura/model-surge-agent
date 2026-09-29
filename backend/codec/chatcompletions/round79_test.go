package chatcompletions

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
)

// 轮次79：chat 响应解码侧「带了来源标注却没产出任何文本块可挂」时整批引用被静默
// 丢弃。流式（decode_stream.go delta.annotations 无 text 槽位的 else 分支）与非流式
// （attachCitations 找不到文本块原样返回）两路都丢，且都不出注记。gemini 解码侧对
// 完全相同的情形（候选带引用却无文本块）经 codec.DroppedCitationsNote 报出，chat
// 独缺——违规则 c（跨族同损同报）与规则 b（chat 自身流式/非流式此前都不报，补齐后
// 须同措辞）。修复：两路计数，共用 codec.DroppedCitationsNote 报出。
//
// 指纹子串 "no text block to attach" 为 DroppedCitationsNote 独有。

const r79fingerprint = "no text block to attach"

// findNote 返回首条含 sub 子串的注记原文，用于逐字比对两路措辞。
func findNote(notes []string, sub string) string {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return n
		}
	}
	return ""
}

// 非流式：message.content 为 null（无文本块）却带 annotations → 引用整批丢弃，报出。
func TestNonStreamDroppedCitationsNoted(t *testing.T) {
	body := `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{
      "role":"assistant","content":null,
      "annotations":[{"type":"url_citation","url_citation":{"url":"https://wx.test/1","start_index":0,"end_index":2}}]}}]}`
	resp, notes, err := DecodeResponseLossy([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	// 引用确实没挂上（无文本块），但丢弃必须报出。
	for _, b := range resp.Content {
		if len(b.Citations) != 0 {
			t.Fatalf("无文本块时引用不应被挂上：%+v", b)
		}
	}
	if !anyNoteHas(notes, codec.DroppedCitationsNote(1)) {
		t.Errorf("无文本块丢弃引用应报 DroppedCitationsNote(1)：%q", notes)
	}
}

// 非流式：有文本块时引用正常挂上，不误报丢弃。
func TestNonStreamCitationsAttachedNoNote(t *testing.T) {
	body := `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{
      "role":"assistant","content":"北京今天晴，明天有雨。",
      "annotations":[{"type":"url_citation","url_citation":{"url":"https://wx.test/1","start_index":6,"end_index":10}}]}}]}`
	resp, notes, err := DecodeResponseLossy([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	attached := 0
	for _, b := range resp.Content {
		attached += len(b.Citations)
	}
	if attached != 1 {
		t.Fatalf("有文本块时引用应挂上，得到 %d 条", attached)
	}
	if anyNoteHas(notes, r79fingerprint) {
		t.Errorf("引用挂上了不应报丢弃：%q", notes)
	}
}

// 规则 b：同一「无文本块丢弃引用」损类，流式与非流式措辞逐字一致（共用同一 helper）。
func TestDroppedCitationsStreamingMatchesNonStreaming(t *testing.T) {
	// 流式：只发 annotations、不发正文 → 无 text 槽位，引用被丢。
	dec := newStreamDecoder()
	if _, err := dec.Feed("", chunk(`{"annotations":[{"type":"url_citation","url_citation":{
      "url":"https://wx.test/1","start_index":0,"end_index":2}}]}`)); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	streamNote := findNote(dec.Notes(), r79fingerprint)

	body := `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{
      "role":"assistant","content":null,
      "annotations":[{"type":"url_citation","url_citation":{"url":"https://wx.test/1","start_index":0,"end_index":2}}]}}]}`
	_, nonStreamNotes, err := DecodeResponseLossy([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	nonStreamNote := findNote(nonStreamNotes, r79fingerprint)

	if streamNote == "" || nonStreamNote == "" {
		t.Fatalf("两条路径都应报出丢弃引用注记：stream=%q nonstream=%q", streamNote, nonStreamNote)
	}
	if streamNote != nonStreamNote {
		t.Errorf("流式与非流式措辞不一致：\n stream=%q\n nonstream=%q", streamNote, nonStreamNote)
	}
}
