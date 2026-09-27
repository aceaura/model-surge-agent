package responses

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 本文件钉住候选 D：responses 的「非 url_citation」标注（file_citation /
// container_file_citation / file_path）在解码侧被跳过时，必须经有损注记报出，
// 不得静默丢弃。覆盖三条解码通道——流式增量帧、流式终态快照、非流式整流，
// 外加请求回声路径（客户端把上一轮标注当历史发回来）。
//
// 判据统一：注记里出现 NonURLCitationDropNote 的指纹子串。措辞与编码侧的
// CitationDropNote（「IR 已有引用、出站渲染不下」）刻意不同，避免混淆。

const nonURLNoteFingerprint = "non-URL annotation"

// fileCitationAnnotation 是官方 file_citation 标注形态：来源身份是 file_id +
// 文件内偏移，没有 url 字段。IR.Citation 以 URL 为身份，装不下。
const fileCitationAnnotation = `{"type":"file_citation","file_id":"file_abc",` +
	`"filename":"report.pdf","start_index":6,"end_index":10}`

// feedRawNotes 与 feedRaw 同款喂帧，但额外回吐解码器收尾时的 Notes()，
// 供注记断言用（feedRaw 只回事件、拿不到注记）。
func feedRawNotes(t *testing.T, raw ...string) ([]ir.Event, []string) {
	t.Helper()
	d := newStreamDecoder()
	var out []ir.Event
	for _, r := range raw {
		evs, err := d.Feed("", r)
		if err != nil {
			t.Fatalf("Feed(%s): %v", r, err)
		}
		out = append(out, evs...)
	}
	out = append(out, d.Finish()...)
	return out, d.Notes()
}

// 流式增量帧路径：annotation.added 携带 file_citation。此前整帧被 decodeAnnotations
// 过滤成空 cs、走 len==0 早返回静默丢弃；现在计数必须在早返回前记下并报出。
func TestStreamNonURLCitationAnnotationAdded(t *testing.T) {
	_, notes := feedRawNotes(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"m1","role":"assistant"}}`,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"`+citeFixture+`"}`,
		`{"type":"response.output_text.annotation.added","output_index":0,"content_index":0,"annotation":`+fileCitationAnnotation+`}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	if !anyNoteHas(notes, nonURLNoteFingerprint) {
		t.Fatalf("file_citation on annotation.added must be noted, got %v", notes)
	}
	// 计数应为 1：注记措辞带条数。
	if !anyNoteHas(notes, codec.NonURLCitationDropNote(1)) {
		t.Errorf("want a count of exactly 1, got %v", notes)
	}
}

// 流式终态快照路径：漏发 annotation.added 的网关，标注只在 content_part.done
// 的 part 快照里出现。doneCitations 必须同样计数并报出。
func TestStreamNonURLCitationFromDoneSnapshot(t *testing.T) {
	_, notes := feedRawNotes(t,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"`+citeFixture+`"}`,
		`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"output_text","text":"`+citeFixture+`","annotations":[`+fileCitationAnnotation+`]}}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	if !anyNoteHas(notes, nonURLNoteFingerprint) {
		t.Fatalf("file_citation in done snapshot must be noted, got %v", notes)
	}
}

// 混合：一个 part 同时带一条 url_citation（保留、发 EvCitation）与一条
// file_citation（跳过、计数）。两条口径互不干扰。
func TestStreamMixedURLAndNonURLCitation(t *testing.T) {
	evs, notes := feedRawNotes(t,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"`+citeFixture+`"}`,
		`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"output_text","text":"`+citeFixture+`","annotations":[`+citeAnnotation+`,`+fileCitationAnnotation+`]}}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	if n := countType(evs, ir.EvCitation); n != 1 {
		t.Fatalf("url_citation must still emit 1 EvCitation, got %d", n)
	}
	if !anyNoteHas(notes, codec.NonURLCitationDropNote(1)) {
		t.Errorf("the file_citation half must be noted with count 1, got %v", notes)
	}
}

// 非流式整流路径：DecodeResponseLossy 扫描 output 里的 output_text part，
// 其中的 file_citation 计数经 notes 回吐。
func TestNonStreamingNonURLCitationLossy(t *testing.T) {
	body := `{"id":"r1","object":"response","status":"completed","output":[
      {"type":"message","role":"assistant","status":"completed","content":[
        {"type":"output_text","text":"` + citeFixture + `","annotations":[` + fileCitationAnnotation + `]}]}]}`
	_, notes, err := DecodeResponseLossy([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if !anyNoteHas(notes, nonURLNoteFingerprint) {
		t.Fatalf("file_citation must be noted on non-streaming decode, got %v", notes)
	}
}

// 请求回声路径：客户端把上一轮 assistant 的 output_text（带 file_citation）当
// 历史发回来。解码侧经 ir.Request.DecodeNotes 报出，不得静默丢弃。
func TestRequestEchoNonURLCitation(t *testing.T) {
	body := `{"model":"gpt-x","input":[
      {"type":"message","role":"assistant","content":[
        {"type":"output_text","text":"` + citeFixture + `","annotations":[` + fileCitationAnnotation + `]}]}]}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if !anyNoteHas(req.DecodeNotes, nonURLNoteFingerprint) {
		t.Fatalf("echoed file_citation must land in DecodeNotes, got %v", req.DecodeNotes)
	}
	// url_citation 那条（若有）应正常进 IR.Citation；这里只发了 file_citation，
	// 故文本块存在但无 Citations。
	if len(req.Messages) == 0 {
		t.Fatalf("assistant message lost: %+v", req.Messages)
	}
}

// 请求回声路径经由 function_call_output 的 part 数组：decodeToolCallOutput 同样
// 要把计数透传出来。
func TestRequestEchoNonURLCitationViaToolOutput(t *testing.T) {
	body := `{"model":"gpt-x","input":[
      {"type":"function_call_output","call_id":"c1","output":[
        {"type":"output_text","text":"` + citeFixture + `","annotations":[` + fileCitationAnnotation + `]}]}]}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if !anyNoteHas(req.DecodeNotes, nonURLNoteFingerprint) {
		t.Fatalf("file_citation in tool output must be noted, got %v", req.DecodeNotes)
	}
}

// 负例：纯 url_citation 不得触发本注记（避免假阳性，与 logprobs 注记同款纪律）。
func TestNoNonURLCitationNoteForPlainURL(t *testing.T) {
	_, notes := feedRawNotes(t,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"`+citeFixture+`"}`,
		`{"type":"response.output_text.annotation.added","output_index":0,"content_index":0,"annotation":`+citeAnnotation+`}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	if anyNoteHas(notes, nonURLNoteFingerprint) {
		t.Fatalf("plain url_citation must not trigger the non-URL note, got %v", notes)
	}
}

// 负例：空 URL 的 url_citation 是空壳，按既有纪律跳过但不计入 non-URL 计数
// （措辞分账：那是「形态装得下但字段空」，不是「另一种引用形态」）。
func TestEmptyURLNotCountedAsNonURL(t *testing.T) {
	_, notes := feedRawNotes(t,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"`+citeFixture+`"}`,
		`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"output_text","text":"`+citeFixture+`","annotations":[{"type":"url_citation","url":"","start_index":1,"end_index":2}]}}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	if anyNoteHas(notes, nonURLNoteFingerprint) {
		t.Fatalf("empty-URL url_citation must not be counted as non-URL, got %v", notes)
	}
}

// 注记按整流去重：同一 part 的多条 file_citation 只报一条带合计数的注记，
// 不逐帧刷多条（与既有 Notes() 去重口径一致）。
func TestNonURLCitationNoteDeduped(t *testing.T) {
	two := `[` + fileCitationAnnotation + `,{"type":"container_file_citation","file_id":"f2","container_file_id":"cf2","start_index":0,"end_index":3}]`
	_, notes := feedRawNotes(t,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"`+citeFixture+`"}`,
		`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"output_text","text":"`+citeFixture+`","annotations":`+two+`}}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	hits := 0
	for _, n := range notes {
		if strings.Contains(n, nonURLNoteFingerprint) {
			hits++
		}
	}
	if hits != 1 {
		t.Fatalf("want exactly 1 deduped non-URL note, got %d (%v)", hits, notes)
	}
	if !anyNoteHas(notes, codec.NonURLCitationDropNote(2)) {
		t.Errorf("want combined count of 2, got %v", notes)
	}
}
