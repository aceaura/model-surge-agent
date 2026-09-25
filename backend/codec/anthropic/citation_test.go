package anthropic

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 正文夹具："明天有雨" 占 rune [6,10)。
const citeText = "北京今天晴，明天有雨。"

func TestCitationsRoundTrip(t *testing.T) {
	body := `{"model":"claude-x","max_tokens":16,"messages":[
      {"role":"user","content":"天气如何"},
      {"role":"assistant","content":[{"type":"text","text":"` + citeText + `","citations":[
        {"type":"web_search_result_location","url":"https://wx.test/1","title":"天气",
         "cited_text":"明天有雨","encrypted_index":"idx1",
         "start_char_index":6,"end_char_index":10}]}]}]}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	cs := req.Messages[1].Content[0].Citations
	if len(cs) != 1 {
		t.Fatalf("want 1 citation, got %+v", cs)
	}
	c := cs[0]
	if c.URL != "https://wx.test/1" || c.Title != "天气" || c.CitedText != "明天有雨" ||
		c.Start != 6 || c.End != 10 || c.EncryptedIndex != "idx1" {
		t.Fatalf("citation decoded wrong: %+v", c)
	}

	raw, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	var w wireRequest
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatalf("re-encoded body not decodable: %v", err)
	}
	var blocks []wireBlock
	if err := json.Unmarshal(w.Messages[1].Content, &blocks); err != nil {
		t.Fatalf("assistant content not a block array: %v", err)
	}
	if len(blocks) != 1 || len(blocks[0].Citations) != 1 {
		t.Fatalf("citations lost on encode: %+v", blocks)
	}
	got := blocks[0].Citations[0]
	if got.Type != "web_search_result_location" || got.URL != "https://wx.test/1" ||
		got.CitedText != "明天有雨" || got.EncryptedIndex != "idx1" ||
		got.StartCharIndex != 6 || got.EndCharIndex != 10 {
		t.Fatalf("citation encoded wrong: %+v", got)
	}
}

// 偏移量键没有 omitempty：start=0 必须真的出现在 JSON 里，
// 否则客户端把 0 和「没有偏移」混为一谈。
func TestEncodeCitationsKeepsZeroStartInJSON(t *testing.T) {
	out := encodeCitations("北京今天晴", []ir.Citation{{
		URL: "https://wx.test/1", CitedText: "北京", Start: 0, End: 2,
	}})
	if len(out) != 1 {
		t.Fatalf("want 1 citation, got %+v", out)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"start_char_index":0`)) {
		t.Errorf("zero start missing from JSON: %s", raw)
	}
	if !bytes.Contains(raw, []byte(`"end_char_index":2`)) {
		t.Errorf("end index missing from JSON: %s", raw)
	}
}

func TestStreamEncodeCitationsKeepsZeroStartInJSON(t *testing.T) {
	enc := newStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvBlockStart, Index: 0,
		Block: &ir.Block{Type: ir.BlockText}}); err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Encode(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "北京"}); err != nil {
		t.Fatal(err)
	}
	frames, err := enc.Encode(ir.Event{Type: ir.EvCitation, Index: 0, Citations: []ir.Citation{{
		URL: "https://wx.test/1", CitedText: "北京", Start: 0, End: 2,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 {
		t.Fatalf("want 1 frame, got %d", len(frames))
	}
	if !bytes.Contains(frames[0], []byte(`"start_char_index":0`)) {
		t.Errorf("zero start missing from frame: %s", frames[0])
	}
}

// 跨协议来的引用常常只有 cited_text：本协议要求偏移量，编码时回推。
func TestEncodeCitationsBackfillsRange(t *testing.T) {
	out := encodeCitations(citeText, []ir.Citation{{
		URL: "https://wx.test/1", CitedText: "明天有雨",
	}})
	if len(out) != 1 {
		t.Fatalf("want 1 citation, got %+v", out)
	}
	if out[0].StartCharIndex != 6 || out[0].EndCharIndex != 10 {
		t.Errorf("range not backfilled: [%d,%d), want [6,10)",
			out[0].StartCharIndex, out[0].EndCharIndex)
	}
}

// 反向同理：只有偏移量没有 cited_text 时按范围切出原文。
func TestEncodeCitationsBackfillsCitedText(t *testing.T) {
	out := encodeCitations("北京今天晴", []ir.Citation{{
		URL: "https://wx.test/1", Start: 0, End: 2,
	}})
	if len(out) != 1 {
		t.Fatalf("want 1 citation, got %+v", out)
	}
	if out[0].CitedText != "北京" {
		t.Errorf("cited_text not backfilled: %q, want 北京", out[0].CitedText)
	}
}

// 定位不了的条目整条丢弃：编出去就是上游 400 拒整轮，比丢一条标注更糟。
func TestEncodeCitationsDropsUnresolvable(t *testing.T) {
	cases := []struct {
		name string
		c    ir.Citation
	}{
		{"无范围无原文", ir.Citation{URL: "https://wx.test/1"}},
		{"原文不在正文里", ir.Citation{URL: "https://wx.test/1", CitedText: "上海多云"}},
		{"范围越界", ir.Citation{URL: "https://wx.test/1", Start: 100, End: 200}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := encodeCitations(citeText, []ir.Citation{tc.c})
			if len(out) != 0 {
				t.Fatalf("unresolvable citation must be dropped, got %+v", out)
			}
		})
	}
	// 请求体层面：整块丢空后 citations 键不得出现（omitempty 生效）。
	raw, err := json.Marshal(wireBlock{Type: blockText, Text: citeText,
		Citations: encodeCitations(citeText, []ir.Citation{{URL: "https://wx.test/1"}})})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("citations")) {
		t.Errorf("empty citations key must be omitted: %s", raw)
	}
}

// URL 为空的条目在解码期就丢：它指向不了任何来源，留着只会污染聚合结果。
func TestDecodeCitationsDropsEmptyURL(t *testing.T) {
	out := decodeCitations([]citation{{URL: "", CitedText: "北京"}})
	if out != nil {
		t.Fatalf("want nil, got %+v", out)
	}
}

func TestStreamDecodeCitationsDelta(t *testing.T) {
	dec := newStreamDecoder()
	got, err := dec.Feed(evContentBlockDelta,
		`{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":`+
			`{"type":"web_search_result_location","url":"https://wx.test/1","title":"天气",`+
			`"cited_text":"明天有雨","encrypted_index":"idx1","start_char_index":6,"end_char_index":10}}}`)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(got) != 1 || got[0].Type != ir.EvCitation {
		t.Fatalf("want one EvCitation, got %+v", got)
	}
	cs := got[0].Citations
	if len(cs) != 1 {
		t.Fatalf("want 1 citation, got %+v", cs)
	}
	if cs[0].URL != "https://wx.test/1" || cs[0].CitedText != "明天有雨" ||
		cs[0].Start != 6 || cs[0].End != 10 || cs[0].EncryptedIndex != "idx1" {
		t.Fatalf("citation decoded wrong: %+v", cs[0])
	}
}

// citation 键缺失的畸形帧：静默跳过，不报错也不发零事件。
func TestStreamDecodeCitationsDeltaNil(t *testing.T) {
	dec := newStreamDecoder()
	got, err := dec.Feed(evContentBlockDelta,
		`{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta"}}`)
	if err != nil {
		t.Fatalf("nil citation must not fail the stream: %v", err)
	}
	if got != nil {
		t.Fatalf("want no events, got %+v", got)
	}
}

// 流式编码用已下发的累积正文回推偏移量：客户端手里的正文就是这份，
// 对不上等于标注指错位置。
func TestStreamEncodeCitations(t *testing.T) {
	enc := newStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvBlockStart, Index: 0,
		Block: &ir.Block{Type: ir.BlockText}}); err != nil {
		t.Fatal(err)
	}
	for _, chunk := range []string{"北京今天晴，", "明天有雨。"} {
		if _, err := enc.Encode(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: chunk}); err != nil {
			t.Fatal(err)
		}
	}
	frames, err := enc.Encode(ir.Event{Type: ir.EvCitation, Index: 0, Citations: []ir.Citation{{
		URL: "https://wx.test/1", Title: "天气", CitedText: "明天有雨",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 {
		t.Fatalf("want 1 frame, got %d", len(frames))
	}
	f := string(frames[0])
	if !strings.Contains(f, `"type":"citations_delta"`) {
		t.Errorf("wrong delta type: %s", f)
	}
	if !strings.Contains(f, `"start_char_index":6`) || !strings.Contains(f, `"end_char_index":10`) {
		t.Errorf("range not backfilled from accumulated text: %s", f)
	}
}

// 多条引用逐帧发送：本协议一帧只带一条。
func TestStreamEncodeCitationsOneFrameEach(t *testing.T) {
	enc := newStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvBlockStart, Index: 0,
		Block: &ir.Block{Type: ir.BlockText}}); err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Encode(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: citeText}); err != nil {
		t.Fatal(err)
	}
	frames, err := enc.Encode(ir.Event{Type: ir.EvCitation, Index: 0, Citations: []ir.Citation{
		{URL: "https://wx.test/1", CitedText: "明天有雨"},
		{URL: "https://wx.test/2", CitedText: "北京今天晴"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 {
		t.Fatalf("want 2 frames, got %d", len(frames))
	}
}

// 块没开时引用直接丢：为它凭空补开块等于伪造块结构，客户端会多出空文本块。
func TestStreamEncodeCitationsWithoutOpenBlock(t *testing.T) {
	enc := newStreamEncoder()
	frames, err := enc.Encode(ir.Event{Type: ir.EvCitation, Index: 0, Citations: []ir.Citation{{
		URL: "https://wx.test/1", CitedText: "明天有雨", Start: 6, End: 10,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 0 {
		t.Fatalf("want no frames without an open block, got %d", len(frames))
	}
}

// 历史以 assistant 起头时编码器会补一条占位 user 消息，引用块顺移到
// messages[1]；偏移量 [0,1) 必须原样保留（零起点不得被当成缺失丢掉）。
func TestDecodeRequestCitations(t *testing.T) {
	body := `{"model":"claude-x","max_tokens":16,"messages":[
      {"role":"assistant","content":[{"type":"text","text":"北京今天晴","citations":[
        {"type":"web_search_result_location","url":"https://wx.test/1",
         "cited_text":"北","start_char_index":0,"end_char_index":1}]}]},
      {"role":"user","content":"继续"}]}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	raw, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	var w wireRequest
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatal(err)
	}
	if len(w.Messages) != 3 || w.Messages[0].Role != string(ir.RoleUser) {
		t.Fatalf("want placeholder + 2 messages, got %+v", w.Messages)
	}
	var blocks []wireBlock
	if err := json.Unmarshal(w.Messages[1].Content, &blocks); err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || len(blocks[0].Citations) != 1 {
		t.Fatalf("citations lost: %+v", blocks)
	}
	c := blocks[0].Citations[0]
	if c.StartCharIndex != 0 || c.EndCharIndex != 1 || c.CitedText != "北" {
		t.Fatalf("zero-start citation mangled: %+v", c)
	}
}
