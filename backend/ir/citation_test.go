package ir

import (
	"encoding/json"
	"testing"
)

// 正文夹具："明天有雨" 占 rune [6,10)。
const citeFixture = "北京今天晴，明天有雨。"

func TestResolveCitedTextFromRange(t *testing.T) {
	got := ResolveCitedText("北京今天晴，明天有雨。", Citation{Start: 0, End: 5})
	if got != "北京今天晴" {
		t.Errorf("ResolveCitedText = %q, want 北京今天晴", got)
	}
}

func TestResolveCitedTextPrefersGiven(t *testing.T) {
	got := ResolveCitedText(citeFixture, Citation{CitedText: "上海多云", Start: 0, End: 5})
	if got != "上海多云" {
		t.Errorf("given cited_text must win, got %q", got)
	}
}

func TestResolveCitedTextOutOfRange(t *testing.T) {
	cases := []struct {
		name string
		c    Citation
	}{
		{"无范围", Citation{}},
		{"终点越界", Citation{Start: 6, End: 100}},
		{"负起点", Citation{Start: -1, End: 3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveCitedText(citeFixture, tc.c); got != "" {
				t.Errorf("want empty, got %q", got)
			}
		})
	}
}

func TestResolveRangeFromCitedText(t *testing.T) {
	start, end, ok := ResolveRange(citeFixture, Citation{CitedText: "明天有雨"})
	if !ok || start != 6 || end != 10 {
		t.Errorf("ResolveRange = [%d,%d) %v, want [6,10) true", start, end, ok)
	}
}

// 同一片段出现多次时不猜：高亮落在错误的一处比不高亮更糟。
func TestResolveRangeAmbiguous(t *testing.T) {
	if _, _, ok := ResolveRange("go and go again", Citation{CitedText: "go"}); ok {
		t.Error("ambiguous match must not resolve")
	}
}

func TestResolveRangeNoMatch(t *testing.T) {
	if _, _, ok := ResolveRange(citeFixture, Citation{CitedText: "上海多云"}); ok {
		t.Error("absent cited_text must not resolve")
	}
}

func TestResolveRangePrefersGiven(t *testing.T) {
	start, end, ok := ResolveRange(citeFixture, Citation{CitedText: "明天有雨", Start: 1, End: 2})
	if !ok || start != 1 || end != 2 {
		t.Errorf("given range must win, got [%d,%d) %v", start, end, ok)
	}
}

func TestDedupeCitations(t *testing.T) {
	in := []Citation{
		{URL: "https://a", Start: 0, End: 1},
		{URL: "https://a", Start: 0, End: 1}, // 完全重复
		{URL: "https://b", Start: 0, End: 1}, // URL 不同，保留
		{URL: "https://a", Start: 2, End: 3}, // 范围不同，保留
	}
	got := DedupeCitations(in)
	if len(got) != 3 {
		t.Fatalf("want 3 kept, got %+v", got)
	}
	if got[0].URL != "https://a" || got[1].URL != "https://b" || got[2].Start != 2 {
		t.Errorf("order not preserved: %+v", got)
	}
}

// 去重不承担「丢掉空 URL」的职责：Anthropic 的文档类引用（char_location 等）
// 本来就没有 URL，靠 document_index 与页/块/字符下标定位。此前那一条规则让
// 官方五种形态里的四种在解码后被静默清空，客户端看不到模型引了哪份文档。
func TestDedupeCitationsKeepsURLLess(t *testing.T) {
	out := DedupeCitations([]Citation{{CitedText: "x"}})
	if len(out) != 1 {
		t.Fatalf("空 URL 的引用被丢了：%+v", out)
	}
	if out := DedupeCitations(nil); out != nil {
		t.Fatalf("空输入却返回了 %+v", out)
	}
}

// 带 Raw 的按原文比：文档类引用的 URL 与范围可能全空，只靠投影字段区分会把
// 「同一段文字引自两个不同文档」误判成重复而丢掉一条真实出处。
func TestDedupeCitationsUsesRawAsKey(t *testing.T) {
	out := DedupeCitations([]Citation{
		{CitedText: "晴", Raw: json.RawMessage(`{"type":"char_location","document_index":0}`)},
		{CitedText: "晴", Raw: json.RawMessage(`{"type":"char_location","document_index":0}`)},
		{CitedText: "晴", Raw: json.RawMessage(`{"type":"char_location","document_index":1}`)},
	})
	if len(out) != 2 {
		t.Fatalf("去重后 %d 条，want 2：%+v", len(out), out)
	}
}

// Portable 判据：外族的标注槽位（Chat/Responses 的 url_citation）一律以 URL
// 为来源身份，没有 URL 就无从表达。
func TestCitationPortable(t *testing.T) {
	if (Citation{CitedText: "x"}).Portable() {
		t.Error("无 URL 却判为可跨协议")
	}
	if !(Citation{URL: "https://a"}).Portable() {
		t.Error("有 URL 却判为不可跨协议")
	}
}

// 计数覆盖多消息多块：诊断要报总条数，漏层会让影响面被低估。
// 与 CountCitations 分开数，是因为三个外族「有标注槽位但装不下文档类引用」，
// 整族布尔量看不见这种逐条损耗。
func TestCountNonPortableCitations(t *testing.T) {
	req := &Request{Messages: []Message{
		{Role: RoleAssistant, Content: []Block{
			{Type: BlockText, Text: "a", Citations: []Citation{
				{URL: "https://a"}, {CitedText: "文档引用", WireType: "char_location"},
			}},
			{Type: BlockText, Text: "b", Citations: []Citation{{WireType: "page_location"}}},
		}},
		{Role: RoleAssistant, Content: []Block{{Type: BlockText, Text: "c",
			Citations: []Citation{{URL: "https://d"}}}}},
	}}
	if n := CountNonPortableCitations(req); n != 2 {
		t.Fatalf("CountNonPortableCitations = %d, want 2", n)
	}
}

func TestCountCitations(t *testing.T) {
	r := &Request{Messages: []Message{
		{Role: RoleAssistant, Content: []Block{
			{Type: BlockText, Text: "a", Citations: []Citation{{URL: "https://a"}, {URL: "https://b"}}},
		}},
		{Role: RoleAssistant, Content: []Block{
			{Type: BlockText, Text: "b", Citations: []Citation{{URL: "https://c"}}},
			{Type: BlockText, Text: "c", Citations: []Citation{{URL: "https://d"}}},
		}},
	}}
	if n := CountCitations(r); n != 4 {
		t.Errorf("CountCitations = %d, want 4", n)
	}
}

func TestCitationHasRange(t *testing.T) {
	cases := []struct {
		c    Citation
		want bool
	}{
		{Citation{Start: 0, End: 2}, true},
		{Citation{Start: 3, End: 3}, false},
		{Citation{Start: 5, End: 1}, false},
		{Citation{}, false},
	}
	for _, tc := range cases {
		if got := tc.c.HasRange(); got != tc.want {
			t.Errorf("HasRange(%+v) = %v, want %v", tc.c, got, tc.want)
		}
	}
}

// 引用事件累到已开的文本块上；重复到达时去重。
func TestAggregatorCitation(t *testing.T) {
	a := &Aggregator{}
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{Type: BlockText}})
	a.Add(Event{Type: EvTextDelta, Index: 0, Text: citeFixture})
	a.Add(Event{Type: EvCitation, Index: 0, Citations: []Citation{{URL: "https://a", Start: 6, End: 10}}})
	a.Add(Event{Type: EvCitation, Index: 0, Citations: []Citation{{URL: "https://a", Start: 6, End: 10}}})
	a.Add(Event{Type: EvBlockStop, Index: 0})

	got := a.Response()
	if len(got.Content) != 1 {
		t.Fatalf("citation must not open a new block: %#v", got.Content)
	}
	cs := got.Content[0].Citations
	if len(cs) != 1 || cs[0].URL != "https://a" {
		t.Fatalf("want one deduped citation, got %+v", cs)
	}
	if got.Content[0].Text != citeFixture {
		t.Errorf("text mutated: %q", got.Content[0].Text)
	}
}

// 块没开时引用事件不落地：聚合器不为它凭空建块。
func TestAggregatorCitationNoOpenBlock(t *testing.T) {
	a := &Aggregator{}
	a.Add(Event{Type: EvCitation, Index: 0, Citations: []Citation{{URL: "https://a"}}})
	got := a.Response()
	if len(got.Content) != 0 {
		t.Fatalf("citation without an open block must not create one: %#v", got.Content)
	}
}

// 非流式响应投影成事件时：引用事件排在正文增量之后、块开启帧不带引用
// （客户端按增量拼正文，骨架块带引用会被下游编码器按开启帧入参丢掉同款逻辑忽略）。
func TestEventsFromResponseEmitsCitationAfterText(t *testing.T) {
	resp := &Response{Content: []Block{{
		Type: BlockText, Text: citeFixture,
		Citations: []Citation{{URL: "https://a", Start: 6, End: 10}},
	}}}
	evs := ResponseEvents(resp)
	var startAt, textAt, citeAt = -1, -1, -1
	for i, ev := range evs {
		switch ev.Type {
		case EvBlockStart:
			startAt = i
			if len(ev.Block.Citations) != 0 {
				t.Errorf("block_start must not carry citations: %+v", ev.Block)
			}
		case EvTextDelta:
			textAt = i
		case EvCitation:
			citeAt = i
			if len(ev.Citations) != 1 || ev.Citations[0].URL != "https://a" {
				t.Errorf("citation event payload wrong: %+v", ev.Citations)
			}
		}
	}
	if !(startAt < textAt && textAt < citeAt) {
		t.Fatalf("order must be start < text < citation, got %d %d %d", startAt, textAt, citeAt)
	}
}

func TestEventsFromResponseNoCitationEvent(t *testing.T) {
	resp := &Response{Content: []Block{{Type: BlockText, Text: "plain"}}}
	for _, ev := range ResponseEvents(resp) {
		if ev.Type == EvCitation {
			t.Fatalf("no citations, want no EvCitation: %+v", ev)
		}
	}
}
