package responses

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 正文夹具："明天有雨" 占 rune [6,10)。
const citeFixture = "北京今天晴，明天有雨。"

const citeAnnotation = `{"type":"url_citation","url":"https://wx.test/1","title":"天气",` +
	`"start_index":6,"end_index":10}`

func TestAnnotationsRoundTrip(t *testing.T) {
	body := `{"id":"r1","object":"response","status":"completed","output":[
      {"type":"message","role":"assistant","status":"completed","content":[
        {"type":"output_text","text":"` + citeFixture + `","annotations":[` + citeAnnotation + `]}]}]}`
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
		cs[0].Start != 6 || cs[0].End != 10 {
		t.Fatalf("citation decoded wrong: %+v", cs[0])
	}

	raw, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"annotations"`)) ||
		!bytes.Contains(raw, []byte("https://wx.test/1")) {
		t.Fatalf("annotations lost on encode: %s", raw)
	}
	if !bytes.Contains(raw, []byte(`"start_index":6`)) || !bytes.Contains(raw, []byte(`"end_index":10`)) {
		t.Errorf("range lost on encode: %s", raw)
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
	// 本协议没有 cited_text 字段，不得凭空写出。
	if bytes.Contains(raw, []byte("cited_text")) {
		t.Errorf("responses annotations must not carry cited_text: %s", raw)
	}
}

// 定位不了范围时保留条目：URL 照发，偏移量留零值。
func TestEncodeAnnotationsKeepsRangelessCitation(t *testing.T) {
	as := encodeAnnotations(citeFixture, []ir.Citation{{URL: "https://wx.test/1", Title: "天气"}})
	if len(as) != 1 {
		t.Fatalf("rangeless citation must be kept, got %+v", as)
	}
	if as[0].URL != "https://wx.test/1" || as[0].StartIndex != 0 || as[0].EndIndex != 0 {
		t.Errorf("rangeless annotation wrong: %+v", as[0])
	}
}

// type 过滤：认不出的类型跳过，空 type 放行，空 URL 由去重丢掉。
func TestDecodeAnnotationsFilters(t *testing.T) {
	in := []annotation{
		{Type: "file_citation", URL: "https://file.test/1"},
		{Type: "", URL: "https://wx.test/2", StartIndex: 1, EndIndex: 2},
		{Type: "url_citation", URL: ""},
	}
	got := decodeAnnotations(in)
	if len(got) != 1 {
		t.Fatalf("want 1 kept, got %+v", got)
	}
	if got[0].URL != "https://wx.test/2" {
		t.Errorf("wrong survivor: %+v", got[0])
	}
}

// 回归：annotation.added 曾与 output_text.delta 并成一个 case，而它没有
// delta 字段，整帧被静默丢掉——web 搜索回答的来源标注全部消失。
func TestStreamDecodeAnnotationAdded(t *testing.T) {
	evs := feedRaw(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"m1","role":"assistant"}}`,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"`+citeFixture+`"}`,
		`{"type":"response.output_text.annotation.added","output_index":0,"content_index":0,"annotation":`+citeAnnotation+`}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	var cite *ir.Event
	for i := range evs {
		if evs[i].Type == ir.EvCitation {
			cite = &evs[i]
		}
	}
	if cite == nil {
		t.Fatalf("annotation.added must produce EvCitation: %+v", evs)
	}
	if len(cite.Citations) != 1 || cite.Citations[0].URL != "https://wx.test/1" ||
		cite.Citations[0].Start != 6 || cite.Citations[0].End != 10 {
		t.Fatalf("citation payload wrong: %+v", cite.Citations)
	}
}

// annotation 键缺失的畸形帧：静默跳过，不报错也不发零事件。
func TestStreamDecodeAnnotationAddedNil(t *testing.T) {
	evs := feedRaw(t,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hi"}`,
		`{"type":"response.output_text.annotation.added","output_index":0,"content_index":0}`)
	if n := countType(evs, ir.EvCitation); n != 0 {
		t.Fatalf("nil annotation must not emit EvCitation, got %d", n)
	}
}

// done 帧的 part 快照也是标注来源：漏发 annotation.added 的网关全靠它。
func TestStreamDecodeAnnotationsFromDoneSnapshot(t *testing.T) {
	evs := feedRaw(t,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"`+citeFixture+`"}`,
		`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"output_text","text":"`+citeFixture+`","annotations":[`+citeAnnotation+`]}}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	if n := countType(evs, ir.EvCitation); n != 1 {
		t.Fatalf("want 1 EvCitation from done snapshot, got %d (%+v)", n, evs)
	}
}

// 拆分后普通文本增量不受影响。
func TestStreamDecodeTextDeltaUnaffected(t *testing.T) {
	evs := feedRaw(t,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hello"}`)
	found := false
	for _, ev := range evs {
		if ev.Type == ir.EvTextDelta && ev.Text == "hello" {
			found = true
		}
	}
	if !found {
		t.Fatalf("text delta lost: %+v", evs)
	}
}

// 流式编码：增量走 annotation.added 帧（一帧一条），done 条目的 part
// 带全量快照；零值序号必须写出（requiredIndexFields）。
func TestStreamEncodeAnnotations(t *testing.T) {
	raw := encodeAll(t,
		ir.Event{Type: ir.EvMessageStart, MessageID: "r1"},
		ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		ir.Event{Type: ir.EvTextDelta, Index: 0, Text: citeFixture},
		ir.Event{Type: ir.EvCitation, Index: 0, Citations: []ir.Citation{
			{URL: "https://wx.test/1", Title: "天气", CitedText: "明天有雨"},
			{URL: "https://wx.test/2", CitedText: "北京今天晴"},
		}},
		ir.Event{Type: ir.EvBlockStop, Index: 0},
		ir.Event{Type: ir.EvMessageStop})
	// 事件名同时出现在 event: 行与 data 的 type 键里，按 event: 行计数。
	if n := strings.Count(raw, "event: "+evOutputTextAnnotationAdded); n != 2 {
		t.Errorf("want 2 annotation.added frames, got %d:\n%s", n, raw)
	}
	if !strings.Contains(raw, `"start_index":6`) || !strings.Contains(raw, `"end_index":10`) {
		t.Errorf("range not resolved against accumulated text:\n%s", raw)
	}
	// 第一条标注在 output_index=0、content_index=0 上，两个零值都必须写出。
	if !strings.Contains(raw, `"output_index":0`) || !strings.Contains(raw, `"content_index":0`) {
		t.Errorf("zero index fields missing from annotation frames:\n%s", raw)
	}
	// done 条目的 part 要带标注快照：只读终态的客户端全靠它。
	doneAt := strings.Index(raw, `"type":"response.output_item.done"`)
	if doneAt < 0 {
		t.Fatalf("no output_item.done frame:\n%s", raw)
	}
	tail := raw[doneAt:]
	if !strings.Contains(tail, `"annotations"`) || !strings.Contains(tail, "https://wx.test/2") {
		t.Errorf("done snapshot missing annotations:\n%s", tail)
	}
}

// 块没开时引用不凭空补开条目：那会多出一个空 message item 并烧掉 output_index。
func TestStreamEncodeCitationWithoutBlock(t *testing.T) {
	raw := encodeAll(t,
		ir.Event{Type: ir.EvMessageStart, MessageID: "r1"},
		ir.Event{Type: ir.EvCitation, Index: 0, Citations: []ir.Citation{{URL: "https://wx.test/1"}}},
		ir.Event{Type: ir.EvMessageStop})
	if strings.Contains(raw, "event: "+evOutputTextAnnotationAdded) {
		t.Errorf("citation without a block must not emit frames:\n%s", raw)
	}
	if strings.Contains(raw, `"output"`) {
		t.Errorf("no item must be opened:\n%s", raw)
	}
}

// 请求侧往返：历史 item 里的 annotations 解进 IR 再编回来。
func TestRequestAnnotationsRoundTrip(t *testing.T) {
	body := `{"model":"m","input":[
      {"type":"message","role":"user","content":[{"type":"input_text","text":"天气如何"}]},
      {"type":"message","role":"assistant","content":[
        {"type":"output_text","text":"` + citeFixture + `","annotations":[` + citeAnnotation + `]}]}]}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	// assistant item 进 Messages；引用挂在文本块上。
	var cs []ir.Citation
	for _, m := range req.Messages {
		for _, b := range m.Content {
			cs = append(cs, b.Citations...)
		}
	}
	if len(cs) != 1 || cs[0].URL != "https://wx.test/1" || cs[0].Start != 6 || cs[0].End != 10 {
		t.Fatalf("citations not decoded: %+v", cs)
	}
	raw, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"annotations"`)) ||
		!bytes.Contains(raw, []byte(`"start_index":6`)) {
		t.Errorf("annotations lost on request encode: %s", raw)
	}
}
