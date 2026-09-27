package gemini

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// gemini 上游开了 Google Search grounding（或代码引用）时，会在候选上带出
// citationMetadata.citationSources 与 groundingMetadata.groundingChunks，二者都
// 携 URI（+标题）。此前 wireCandidate 未建模这两键、两条解码路径都不读，来源
// 标注被 json.Unmarshal 静默吞掉——IR 的 Citation 槽位（chat 的 annotations、
// anthropic 的 citations 都填它）在 gemini 这一路恒空、且无任何注记。以下钉住
// 保全（URL+标题进 ir.Citation，Portable 为真）、按 URL 去重、字节范围不携带，
// 以及「候选无文本块可挂」时经注记报出而非静默。

// textCitations 返回响应里第一个文本块携带的引用。
func textCitations(t *testing.T, resp *ir.Response) []ir.Citation {
	t.Helper()
	for _, b := range resp.Content {
		if b.Type == ir.BlockText {
			return b.Citations
		}
	}
	return nil
}

// 非流式：groundingMetadata 的 web 来源映进文本块的 Citations（URL+标题）。
func TestGroundingCitationsDecodedNonStreaming(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"Paris is the capital of France."}]},` +
		`"groundingMetadata":{"groundingChunks":[{"web":{"uri":"https://example.com/paris","title":"Paris - Wikipedia"}}]}}],` +
		`"usageMetadata":{"candidatesTokenCount":8}}`)
	resp, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	cs := textCitations(t, resp)
	if len(cs) != 1 {
		t.Fatalf("引用数 = %d, want 1: %+v", len(cs), cs)
	}
	if cs[0].URL != "https://example.com/paris" || cs[0].Title != "Paris - Wikipedia" {
		t.Errorf("引用内容不对: %+v", cs[0])
	}
	if !cs[0].Portable() {
		t.Errorf("带 URL 的引用应可移植: %+v", cs[0])
	}
	if cs[0].HasRange() {
		t.Errorf("gemini 字节偏移不应携带为 IR rune 范围: %+v", cs[0])
	}
	for _, n := range notes {
		if strings.Contains(n, "citation") {
			t.Errorf("成功保全时不应有丢弃注记: %q", n)
		}
	}
}

// 非流式：citationMetadata.citationSources 的 uri 也映进 Citations。
func TestCitationMetadataSourcesDecodedNonStreaming(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"answer"}]},` +
		`"citationMetadata":{"citationSources":[` +
		`{"uri":"https://a.example/1","startIndex":0,"endIndex":6},` +
		`{"uri":"https://b.example/2"}]}}]}`)
	resp, _, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	cs := textCitations(t, resp)
	if len(cs) != 2 {
		t.Fatalf("引用数 = %d, want 2: %+v", len(cs), cs)
	}
	if cs[0].URL != "https://a.example/1" || cs[1].URL != "https://b.example/2" {
		t.Errorf("citationSources 映射不对: %+v", cs)
	}
	// 字节偏移不携带为范围。
	if cs[0].HasRange() {
		t.Errorf("citationSources 的字节偏移不应变成 IR 范围: %+v", cs[0])
	}
}

// 同一 URI 同时出现在 citationSources 与 groundingChunks：按 URL 去重成一条。
func TestCitationsDedupeByURL(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"x"}]},` +
		`"citationMetadata":{"citationSources":[{"uri":"https://dup.example"}]},` +
		`"groundingMetadata":{"groundingChunks":[{"web":{"uri":"https://dup.example","title":"T"}}]}}]}`)
	resp, _, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	cs := textCitations(t, resp)
	if len(cs) != 1 {
		t.Fatalf("去重后应只剩 1 条，得到 %d: %+v", len(cs), cs)
	}
	// 先到的 citationSources 无标题，后到的 grounding 有标题；去重保留先到者。
	if cs[0].URL != "https://dup.example" {
		t.Errorf("URL 不对: %+v", cs[0])
	}
}

// retrievedContext 块（文件检索工具）带 uri/title/text：text 进 CitedText。
func TestRetrievedContextCitationNonStreaming(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"x"}]},` +
		`"groundingMetadata":{"groundingChunks":[{"retrievedContext":` +
		`{"uri":"https://files.example/doc","title":"Report","text":"quoted snippet"}}]}}]}`)
	resp, _, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	cs := textCitations(t, resp)
	if len(cs) != 1 || cs[0].URL != "https://files.example/doc" ||
		cs[0].Title != "Report" || cs[0].CitedText != "quoted snippet" {
		t.Fatalf("retrievedContext 映射不对: %+v", cs)
	}
}

// 候选只有函数调用、没有文本块：引用无处可挂，经注记报出而非静默丢弃。
func TestCitationsNoTextBlockNotedNonStreaming(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"functionCall":{"name":"f","args":{}}}]},` +
		`"groundingMetadata":{"groundingChunks":[{"web":{"uri":"https://x.example"}}]}}]}`)
	resp, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if len(textCitations(t, resp)) != 0 {
		t.Errorf("无文本块时不应有引用挂上")
	}
	var noted bool
	for _, n := range notes {
		if strings.Contains(n, "citation") {
			noted = true
		}
	}
	if !noted {
		t.Errorf("无文本块可挂的引用应经注记报出，notes=%q", notes)
	}
}

// 流式：来源标注随候选帧到达，挂到已开的文本块上，聚合后落进 Citations。
func TestGroundingCitationsDecodedStreaming(t *testing.T) {
	dec := newStreamDecoder()
	var agg ir.Aggregator
	feed := func(data string) {
		evs, err := dec.Feed("", data)
		if err != nil {
			t.Fatalf("Feed: %v", err)
		}
		for _, ev := range evs {
			agg.Add(ev)
		}
	}
	// 正文先到、grounding 在收尾帧带一次（gemini 的典型形态）。
	feed(`{"candidates":[{"content":{"parts":[{"text":"Paris is the capital."}]}}]}`)
	feed(`{"candidates":[{"content":{"parts":[{"text":"Paris is the capital."}]},"finishReason":"STOP",` +
		`"groundingMetadata":{"groundingChunks":[{"web":{"uri":"https://example.com/paris","title":"Paris"}}]}}]}`)
	for _, ev := range dec.Finish() {
		agg.Add(ev)
	}
	cs := textCitations(t, agg.Response())
	if len(cs) != 1 || cs[0].URL != "https://example.com/paris" || cs[0].Title != "Paris" {
		t.Fatalf("流式引用保全不对: %+v", cs)
	}
	if !cs[0].Portable() {
		t.Errorf("流式引用应可移植: %+v", cs[0])
	}
}

// 流式：候选无文本块（纯函数调用）时引用无处挂，经 Notes() 报出。
func TestCitationsNoTextBlockNotedStreaming(t *testing.T) {
	dec := newStreamDecoder()
	if _, err := dec.Feed("", `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":{}}}]},`+
		`"groundingMetadata":{"groundingChunks":[{"web":{"uri":"https://x.example"}}]}}]}`); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	for _, ev := range dec.Finish() {
		_ = ev
	}
	var noted bool
	for _, n := range dec.Notes() {
		if strings.Contains(n, "citation") {
			noted = true
		}
	}
	if !noted {
		t.Errorf("流式无文本块可挂的引用应经 Notes() 报出，notes=%q", dec.Notes())
	}
}

// 上游没给任何来源标注：不产生引用、不产生丢弃注记。
func TestNoCitationsStaysClean(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"plain"}]}}]}`)
	resp, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if len(textCitations(t, resp)) != 0 {
		t.Errorf("无来源标注时不应有引用")
	}
	for _, n := range notes {
		if strings.Contains(n, "citation") {
			t.Errorf("无来源标注时不应有引用注记: %q", n)
		}
	}
}
