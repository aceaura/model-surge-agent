package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 非流式：跨协议投影来、无 Raw 又反推不出 cited_text 的引用被 encodeBlock 整条
// 丢弃，必须由 EncodeResponseLossy 报出——否则客户端看不到「模型引了某个来源，
// 但那条来源没能带过来」，丢弃成了静默的。
func TestResponseLossyReportsUnresolvableCitation(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{
		Type: ir.BlockText, Text: citeText,
		// 只有 URL、没有 cited_text 也没有范围：ResolveCitedText 返回空 → 丢弃。
		Citations: []ir.Citation{{URL: "https://wx.test/1"}},
	}}}
	_, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !hasNoteSubstr(notes, "cited_text could not be resolved") {
		t.Errorf("缺反推失败引用注记：%#v", notes)
	}
}

// 非流式：能带出去的引用（自带 cited_text，或范围可反推）不得误报丢弃。
func TestResponseLossyKeepsResolvableCitation(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{
		Type: ir.BlockText, Text: citeText,
		Citations: []ir.Citation{
			{URL: "https://wx.test/1", CitedText: "明天有雨"},
			{URL: "https://wx.test/2", Start: 0, End: 2},
		},
	}}}
	_, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatal(err)
	}
	if hasNoteSubstr(notes, "cited_text could not be resolved") {
		t.Errorf("可反推的引用被误报为丢弃：%#v", notes)
	}
}

// 非流式：带 Raw 的引用一律原样透传，不参与反推，也就不会被计为丢弃。
func TestResponseLossyRawCitationNotCounted(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{
		Type: ir.BlockText, Text: citeText,
		Citations: []ir.Citation{{
			URL: "https://w", CitedText: "",
			Raw: []byte(`{"type":"web_search_result_location","url":"https://w","cited_text":"晴"}`),
		}},
	}}}
	_, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatal(err)
	}
	if hasNoteSubstr(notes, "cited_text could not be resolved") {
		t.Errorf("带 Raw 的引用被误报为丢弃：%#v", notes)
	}
}

// 流式：同样的丢弃经 Notes() 报出，判据与非流式同源。
func TestStreamReportsUnresolvableCitation(t *testing.T) {
	enc := inboundCodec{}.NewStreamEncoder(nil)
	encodeEvent(t, enc, ir.Event{Type: ir.EvBlockStart, Index: 0,
		Block: &ir.Block{Type: ir.BlockText}})
	encodeEvent(t, enc, ir.Event{Type: ir.EvTextDelta, Index: 0, Text: citeText})
	encodeEvent(t, enc, ir.Event{Type: ir.EvCitation, Index: 0,
		Citations: []ir.Citation{{URL: "https://wx.test/1"}}})

	notes := encoderNotes(t, enc)
	if !hasNoteSubstr(notes, "cited_text could not be resolved") {
		t.Errorf("流式缺反推失败引用注记：%#v", notes)
	}
}

// 流式：能反推的引用不得误报。
func TestStreamKeepsResolvableCitation(t *testing.T) {
	enc := inboundCodec{}.NewStreamEncoder(nil)
	encodeEvent(t, enc, ir.Event{Type: ir.EvBlockStart, Index: 0,
		Block: &ir.Block{Type: ir.BlockText}})
	encodeEvent(t, enc, ir.Event{Type: ir.EvTextDelta, Index: 0, Text: citeText})
	encodeEvent(t, enc, ir.Event{Type: ir.EvCitation, Index: 0,
		Citations: []ir.Citation{{URL: "https://wx.test/1", CitedText: "明天有雨"}}})

	if notes := encoderNotes(t, enc); hasNoteSubstr(notes, "cited_text could not be resolved") {
		t.Errorf("流式可反推引用被误报为丢弃：%#v", notes)
	}
}

func hasNoteSubstr(notes []string, sub string) bool {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}
