package chatcompletions

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 正文夹具："明天有雨" 占 rune [6,10)。
const citeFixture = "北京今天晴，明天有雨。"

func TestAnnotationsRoundTrip(t *testing.T) {
	body := `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{
      "role":"assistant","content":"` + citeFixture + `","annotations":[
        {"type":"url_citation","url_citation":{"url":"https://wx.test/1","title":"天气",
         "start_index":6,"end_index":10,"cited_text":"明天有雨"}}]},
      "finish_reason":"stop"}]}`
	resp, err := DecodeResponse([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(resp.Content) != 1 {
		t.Fatalf("want 1 block, got %#v", resp.Content)
	}
	cs := resp.Content[0].Citations
	if len(cs) != 1 {
		t.Fatalf("want 1 citation, got %+v", cs)
	}
	if cs[0].URL != "https://wx.test/1" || cs[0].Title != "天气" ||
		cs[0].Start != 6 || cs[0].End != 10 || cs[0].CitedText != "明天有雨" {
		t.Fatalf("citation decoded wrong: %+v", cs[0])
	}

	raw, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	var w wireResponse
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatal(err)
	}
	as := w.Choices[0].Message.Annotations
	if len(as) != 1 || as[0].Type != "url_citation" || as[0].URLCitation == nil {
		t.Fatalf("annotations lost on encode: %s", raw)
	}
	uc := as[0].URLCitation
	if uc.URL != "https://wx.test/1" || uc.StartIndex != 6 || uc.EndIndex != 10 ||
		uc.CitedText != "明天有雨" {
		t.Fatalf("annotation encoded wrong: %+v", uc)
	}
}

// 偏移量键没有 omitempty：start=0 必须真的出现在 JSON 里。
func TestEncodeAnnotationsKeepsZeroStart(t *testing.T) {
	as := encodeAnnotations("北京今天晴", []ir.Citation{{
		URL: "https://wx.test/1", CitedText: "北京", Start: 0, End: 2,
	}})
	if len(as) != 1 {
		t.Fatalf("want 1 annotation, got %+v", as)
	}
	raw, err := json.Marshal(as)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"start_index":0`)) {
		t.Errorf("zero start missing from JSON: %s", raw)
	}
}

func TestEncodeResponseKeepsZeroStartInJSON(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{
		Type: ir.BlockText, Text: "北京今天晴",
		Citations: []ir.Citation{{URL: "https://wx.test/1", CitedText: "北京", Start: 0, End: 2}},
	}}}
	raw, err := EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"start_index":0`)) {
		t.Errorf("zero start missing from response JSON: %s", raw)
	}
}

// 本协议允许无范围标注：定位不了范围时保留条目，偏移量留零值。
func TestEncodeAnnotationsKeepsRangelessCitation(t *testing.T) {
	as := encodeAnnotations(citeFixture, []ir.Citation{{URL: "https://wx.test/1", Title: "天气"}})
	if len(as) != 1 || as[0].URLCitation == nil {
		t.Fatalf("rangeless citation must be kept, got %+v", as)
	}
	uc := as[0].URLCitation
	if uc.URL != "https://wx.test/1" || uc.StartIndex != 0 || uc.EndIndex != 0 {
		t.Errorf("rangeless annotation wrong: %+v", uc)
	}
}

// file_citation 等其他类型没有 url_citation 子对象，跳过；空 URL 由去重丢掉。
func TestDecodeAnnotationsRejectsUnknownType(t *testing.T) {
	cases := []struct {
		name string
		in   []annotation
	}{
		{"file_citation", []annotation{{Type: "file_citation"}}},
		{"url_citation 但子对象缺失", []annotation{{Type: "url_citation"}}},
		{"空 URL", []annotation{{Type: "url_citation", URLCitation: &urlCitation{URL: ""}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeAnnotations(tc.in); got != nil {
				t.Errorf("want nil, got %+v", got)
			}
		})
	}
}

// 标注挂到最后一个文本块；没有文本块时整批丢弃。
func TestAttachCitations(t *testing.T) {
	blocks := []ir.Block{
		{Type: ir.BlockText, Text: "第一段"},
		{Type: ir.BlockText, Text: "第二段"},
	}
	got := attachCitations(blocks, []ir.Citation{{URL: "https://a"}})
	if len(got[0].Citations) != 0 {
		t.Errorf("first block must stay clean: %+v", got[0])
	}
	if len(got[1].Citations) != 1 {
		t.Errorf("want citation on last text block: %+v", got[1])
	}

	mediaOnly := attachCitations([]ir.Block{{Type: ir.BlockImage}}, []ir.Citation{{URL: "https://a"}})
	if len(mediaOnly[0].Citations) != 0 {
		t.Errorf("no text block, citations must be dropped: %+v", mediaOnly)
	}
}

// 多块正文时各块内的偏移量要平移到消息拼接文本坐标系。
// 前块 6 个 rune，本块的 [0,4) 变成 [6,10)。
func TestEncodeResponseShiftsOffsetsAcrossBlocks(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{
		{Type: ir.BlockText, Text: "北京今天晴，"},
		{Type: ir.BlockText, Text: "明天有雨。",
			Citations: []ir.Citation{{URL: "https://wx.test/1", CitedText: "明天有雨"}}},
	}}
	raw, err := EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	var w wireResponse
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatal(err)
	}
	as := w.Choices[0].Message.Annotations
	if len(as) != 1 {
		t.Fatalf("want 1 annotation, got %s", raw)
	}
	if as[0].URLCitation.StartIndex != 6 || as[0].URLCitation.EndIndex != 10 {
		t.Errorf("offsets not shifted: [%d,%d), want [6,10)",
			as[0].URLCitation.StartIndex, as[0].URLCitation.EndIndex)
	}
	if w.Choices[0].Message.Content == nil ||
		!bytes.Contains(w.Choices[0].Message.Content, []byte("北京今天晴，明天有雨。")) {
		t.Errorf("content not concatenated: %s", w.Choices[0].Message.Content)
	}
}

// 请求侧同款平移。两个内容相同的块：引用先在自己的块内解析（块内定位精确），
// 第二块的 [0,4) 平移到 [5,9)。
func TestEncodeRequestShiftsOffsetsAcrossBlocks(t *testing.T) {
	req := &ir.Request{Model: "m", Messages: []ir.Message{{
		Role: ir.RoleAssistant,
		Content: []ir.Block{
			{Type: ir.BlockText, Text: "明天有雨。",
				Citations: []ir.Citation{{URL: "https://wx.test/1", CitedText: "明天有雨"}}},
			{Type: ir.BlockText, Text: "明天有雨。",
				Citations: []ir.Citation{{URL: "https://wx.test/2", CitedText: "明天有雨"}}},
		},
	}}}
	raw, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var w wireRequest
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatal(err)
	}
	as := w.Messages[0].Annotations
	if len(as) != 2 {
		t.Fatalf("want 2 annotations, got %s", raw)
	}
	if as[0].URLCitation.StartIndex != 0 || as[0].URLCitation.EndIndex != 4 {
		t.Errorf("first block range wrong: [%d,%d), want [0,4)",
			as[0].URLCitation.StartIndex, as[0].URLCitation.EndIndex)
	}
	if as[1].URLCitation.StartIndex != 5 || as[1].URLCitation.EndIndex != 9 {
		t.Errorf("second block range not shifted: [%d,%d), want [5,9)",
			as[1].URLCitation.StartIndex, as[1].URLCitation.EndIndex)
	}
}

// 定位不了范围时不丢条目：URL 照常给出，偏移量留零值。
func TestEncodeResponseKeepsSourceWhenUnlocatable(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{
		Type: ir.BlockText, Text: citeFixture,
		Citations: []ir.Citation{{URL: "https://wx.test/1", Title: "天气"}},
	}}}
	raw, err := EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("https://wx.test/1")) {
		t.Errorf("unlocatable source must keep its URL: %s", raw)
	}
	if !bytes.Contains(raw, []byte(`"start_index":0`)) {
		t.Errorf("zero offsets must be written: %s", raw)
	}
}

func chunk(delta string) string {
	return `{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":` + delta + `}]}`
}

func TestStreamDecodeAnnotations(t *testing.T) {
	dec := newStreamDecoder()
	if _, err := dec.Feed("", chunk(`{"content":"`+citeFixture+`"}`)); err != nil {
		t.Fatal(err)
	}
	got, err := dec.Feed("", chunk(`{"annotations":[{"type":"url_citation","url_citation":{
      "url":"https://wx.test/1","title":"天气","start_index":6,"end_index":10}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var cites []ir.Event
	for _, ev := range got {
		if ev.Type == ir.EvCitation {
			cites = append(cites, ev)
		}
	}
	if len(cites) != 1 {
		t.Fatalf("want one EvCitation, got %+v", got)
	}
	if cites[0].Index != 0 || len(cites[0].Citations) != 1 ||
		cites[0].Citations[0].URL != "https://wx.test/1" {
		t.Fatalf("citation event wrong: %+v", cites[0])
	}
}

// 没收到过正文就来的标注不分配块：客户端会多出一个空文本块。
func TestStreamDecodeAnnotationsWithoutTextBlock(t *testing.T) {
	dec := newStreamDecoder()
	got, err := dec.Feed("", chunk(`{"annotations":[{"type":"url_citation","url_citation":{
      "url":"https://wx.test/1","start_index":0,"end_index":2}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range got {
		if ev.Type == ir.EvCitation || ev.Type == ir.EvBlockStart {
			t.Fatalf("annotations without text must not open blocks or emit citations: %+v", got)
		}
	}
}

func TestStreamEncodeAnnotations(t *testing.T) {
	enc := newStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Encode(ir.Event{Type: ir.EvBlockStart, Index: 0,
		Block: &ir.Block{Type: ir.BlockText}}); err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Encode(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: citeFixture}); err != nil {
		t.Fatal(err)
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
	if !strings.Contains(f, `"annotations"`) || !strings.Contains(f, "https://wx.test/1") {
		t.Errorf("annotations missing from frame: %s", f)
	}
	if !strings.Contains(f, `"start_index":6`) || !strings.Contains(f, `"end_index":10`) {
		t.Errorf("range not resolved against accumulated text: %s", f)
	}
}

// 全空的标注批次不发帧：发一个空 annotations 数组会覆盖客户端已累积的标注。
func TestStreamEncodeEmptyCitations(t *testing.T) {
	enc := newStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "c1"}); err != nil {
		t.Fatal(err)
	}
	frames, err := enc.Encode(ir.Event{Type: ir.EvCitation, Index: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 0 {
		t.Fatalf("want no frames, got %d", len(frames))
	}
}

// 请求侧往返：assistant 历史里的 annotations 解进 IR 再编回来，逐字段保真。
func TestRequestAnnotationsRoundTrip(t *testing.T) {
	body := `{"model":"m","messages":[
      {"role":"user","content":"天气如何"},
      {"role":"assistant","content":"` + citeFixture + `","annotations":[
        {"type":"url_citation","url_citation":{"url":"https://wx.test/1","title":"天气",
         "start_index":6,"end_index":10,"cited_text":"明天有雨"}}]},
      {"role":"user","content":"继续"}]}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	cs := req.Messages[1].Content[0].Citations
	if len(cs) != 1 || cs[0].URL != "https://wx.test/1" || cs[0].Start != 6 || cs[0].End != 10 {
		t.Fatalf("citations not decoded into block: %+v", cs)
	}
	raw, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	var w wireRequest
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatal(err)
	}
	as := w.Messages[1].Annotations
	if len(as) != 1 || as[0].URLCitation == nil {
		t.Fatalf("annotations lost on encode: %s", raw)
	}
	uc := as[0].URLCitation
	if uc.URL != "https://wx.test/1" || uc.Title != "天气" ||
		uc.StartIndex != 6 || uc.EndIndex != 10 || uc.CitedText != "明天有雨" {
		t.Fatalf("annotation encoded wrong: %+v", uc)
	}
}
