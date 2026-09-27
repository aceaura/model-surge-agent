package gemini

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// gemini 上游的 Candidate.urlContextMetadata（Output only）携带 url_context
// 工具的检索确认：retrievedUrl + urlRetrievalStatus。SUCCESS/UNSPECIFIED 状态
// 的 URL 是模型实际用到的来源，应映进 ir.Citation（与 groundingChunks.web 同维）；
// ERROR/PAYWALL/UNSAFE 的 URL 模型没读到内容，不算引用。此前 wireCandidate 未建模
// 该键→json.Unmarshal 静默吞掉，与 Round 33 对 citationMetadata/groundingMetadata
// 的保全纪律不一致。

// 非流式：SUCCESS 状态的 retrievedUrl 映进 Citation。
func TestUrlContextSuccessDecodedNonStreaming(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"answer"}]},` +
		`"urlContextMetadata":{"urlMetadata":[` +
		`{"retrievedUrl":"https://example.com/doc","urlRetrievalStatus":"SUCCESS"},` +
		`{"retrievedUrl":"https://paywalled.com","urlRetrievalStatus":"PAYWALL"}` +
		`]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2}}`)
	resp, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	cits := textCitations(t, resp)
	if len(cits) != 1 {
		t.Fatalf("应只采集 SUCCESS 的 1 条，得到 %d: %v", len(cits), cits)
	}
	if cits[0].URL != "https://example.com/doc" {
		t.Errorf("URL=%q", cits[0].URL)
	}
	if !cits[0].Portable() {
		t.Errorf("有 URL 的 Citation 应 Portable")
	}
	// PAYWALL 的不采集，也不应有丢弃注记（它不是引用，模型没用到）
	for _, n := range notes {
		if strings.Contains(n, "paywalled") {
			t.Errorf("PAYWALL URL 不该出现在注记里: %s", n)
		}
	}
}

// 非流式：UNSPECIFIED/空状态视为成功（上游没给状态时默认采集）。
func TestUrlContextUnspecifiedTreatedAsSuccess(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"ok"}]},` +
		`"urlContextMetadata":{"urlMetadata":[` +
		`{"retrievedUrl":"https://a.com","urlRetrievalStatus":"UNSPECIFIED"},` +
		`{"retrievedUrl":"https://b.com"}` +
		`]}}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}`)
	resp, _, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	cits := textCitations(t, resp)
	if len(cits) != 2 {
		t.Fatalf("UNSPECIFIED 和空状态都应采集，得到 %d", len(cits))
	}
}

// 非流式：ERROR/UNSAFE 状态不采集。
func TestUrlContextFailedStatusNotHarvested(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"ok"}]},` +
		`"urlContextMetadata":{"urlMetadata":[` +
		`{"retrievedUrl":"https://err.com","urlRetrievalStatus":"ERROR"},` +
		`{"retrievedUrl":"https://unsafe.com","urlRetrievalStatus":"UNSAFE"}` +
		`]}}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}`)
	resp, _, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	cits := textCitations(t, resp)
	if len(cits) != 0 {
		t.Errorf("ERROR/UNSAFE 不该采集，得到 %d: %v", len(cits), cits)
	}
}

// 非流式：与 groundingMetadata 同 URL 时去重（candidateCitations 的 seen map）。
func TestUrlContextDedupeWithGrounding(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"ok"}]},` +
		`"groundingMetadata":{"groundingChunks":[{"web":{"uri":"https://shared.com","title":"T"}}]},` +
		`"urlContextMetadata":{"urlMetadata":[{"retrievedUrl":"https://shared.com","urlRetrievalStatus":"SUCCESS"}]}` +
		`}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}`)
	resp, _, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	cits := textCitations(t, resp)
	if len(cits) != 1 {
		t.Fatalf("同 URL 应去重为 1，得到 %d", len(cits))
	}
	// grounding 先到，应保留其 title
	if cits[0].Title != "T" {
		t.Errorf("去重后应保留先到条目的 Title，得到 %q", cits[0].Title)
	}
}

// 流式：SUCCESS 的 retrievedUrl 经 EvCitation 挂到文本块。
func TestUrlContextDecodedStreaming(t *testing.T) {
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
	// 正文先到、urlContextMetadata 在收尾帧带一次（gemini 的典型形态）。
	feed(`{"candidates":[{"content":{"parts":[{"text":"answer"}]}}]}`)
	feed(`{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP",` +
		`"urlContextMetadata":{"urlMetadata":[{"retrievedUrl":"https://s.com","urlRetrievalStatus":"SUCCESS"}]}}],` +
		`"usageMetadata":{"candidatesTokenCount":1}}`)
	for _, ev := range dec.Finish() {
		agg.Add(ev)
	}
	cits := textCitations(t, agg.Response())
	if len(cits) != 1 || cits[0].URL != "https://s.com" {
		t.Errorf("流式 urlContext SUCCESS 应保全，得到 %v", cits)
	}
}

// 流式：无文本块时 urlContext 引用计入 droppedCitations 并报出。
func TestUrlContextNoTextBlockNotedStreaming(t *testing.T) {
	dec := newStreamDecoder()
	// 纯函数调用候选 + urlContextMetadata
	_, _ = dec.Feed("", `{"candidates":[{"index":0,"content":{"parts":[{"functionCall":{"name":"f","args":{}}}]},`+
		`"urlContextMetadata":{"urlMetadata":[{"retrievedUrl":"https://x.com","urlRetrievalStatus":"SUCCESS"}]},`+
		`"finishReason":"STOP"}]}`)
	dec.Finish()
	notes := dec.Notes()
	if !anyNoteContains(notes, "citation") {
		t.Errorf("无文本块时 urlContext 引用应报丢弃，得到 %v", notes)
	}
}
