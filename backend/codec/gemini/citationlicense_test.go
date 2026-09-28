package gemini

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
)

// 轮次66：gemini 上游 CitationSource.license（官方 Output only，引用来源的版权/许可
// 标识）此前被 wireCitationSource 建模、却只由 candidateCitations 映 URI 进 ir.Citation，
// license 无 IR 槽位、无注记、也无解释性注释（注释只解释了 startIndex/endIndex 的丢弃），
// 是纯静默丢弃。与轮次50/65 的 safetyRatings 同款「上游给了、IR 无槽位」→ 探测计数经
// CitationLicenseDropNote 报出。gemini 出站-only、跨族恒无 license 维，故只能 NOTE 不能 PRESERVE。
//
// 钉住：规则 a（不漏报：两路径都计数报出；不过报：空/缺席 license 不误报）、
// 规则 b（流式与非流式逐字一致）、计数按「来源条数」而非去重后的引用条数、
// 无正文块（Content==nil 提前 continue）的候选携带的 license 仍被计。

const r66marker = "license/attribution string from"

func r66find(notes []string, marker string) string {
	for _, n := range notes {
		if strings.Contains(n, marker) {
			return n
		}
	}
	return ""
}

// —— 非流式 ——

// 引用来源带 license → 注记报出。
func TestCitationLicenseNotedNonStreaming(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},` +
		`"citationMetadata":{"citationSources":[{"uri":"https://x.example","license":"CC-BY"}]},` +
		`"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if !anyNoteContains(notes, r66marker+" 1 citation source") {
		t.Errorf("非流式 citation license 没被报出：%v", notes)
	}
}

// 多条来源各带 license → 按条数累计。
func TestCitationLicenseCountedNonStreaming(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},` +
		`"citationMetadata":{"citationSources":[` +
		`{"uri":"https://a.example","license":"CC-BY"},` +
		`{"uri":"https://b.example","license":"MIT"}]},` +
		`"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if !anyNoteContains(notes, r66marker+" 2 citation source") {
		t.Errorf("非流式多条 license 计数不对：%v", notes)
	}
}

// 计数按来源条数、不按去重后引用条数：两条同 URL 各带 license → 引用去重成 1，license 计 2。
func TestCitationLicenseCountsPerSourceNotDeduped(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},` +
		`"citationMetadata":{"citationSources":[` +
		`{"uri":"https://dup.example","license":"CC-BY"},` +
		`{"uri":"https://dup.example","license":"MIT"}]},` +
		`"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if !anyNoteContains(notes, r66marker+" 2 citation source") {
		t.Errorf("license 应按来源条数计 2（非去重后 1）：%v", notes)
	}
}

// 无正文块（Content==nil，提前 continue）的候选携带 license → 仍被计数（钉住计数在 continue 之前）。
func TestCitationLicenseCountedWhenNoContentNonStreaming(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,` +
		`"citationMetadata":{"citationSources":[{"uri":"https://x.example","license":"MIT"}]}}],` +
		`"usageMetadata":{"promptTokenCount":5}}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if !anyNoteContains(notes, r66marker+" 1 citation source") {
		t.Errorf("无正文块候选的 license 漏计：%v", notes)
	}
}

// 来源 license 为空串 → 不计数（过报防护）。
func TestCitationLicenseEmptyNotCountedNonStreaming(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},` +
		`"citationMetadata":{"citationSources":[{"uri":"https://x.example","license":""}]},` +
		`"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if anyNoteContains(notes, r66marker) {
		t.Errorf("空 license 被误报：%v", notes)
	}
}

// 来源不带 license 键 → 不计数（过报防护）。
func TestCitationLicenseAbsentStaysCleanNonStreaming(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},` +
		`"citationMetadata":{"citationSources":[{"uri":"https://x.example"}]},` +
		`"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if anyNoteContains(notes, r66marker) {
		t.Errorf("缺席 license 被误报：%v", notes)
	}
}

// 多候选（index>0 被丢）的 license 不计数（只处理 index=0）。
func TestCitationLicenseExtraCandidateNotCounted(t *testing.T) {
	body := []byte(`{"candidates":[` +
		`{"index":0,"content":{"parts":[{"text":"a"}]},"finishReason":"STOP"},` +
		`{"index":1,"content":{"parts":[{"text":"b"}]},` +
		`"citationMetadata":{"citationSources":[{"uri":"https://x.example","license":"MIT"}]}}` +
		`],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2}}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if anyNoteContains(notes, r66marker) {
		t.Errorf("extra candidate 的 license 不该计入：%v", notes)
	}
}

// —— 流式 ——

// 引用来源带 license → Notes() 报出。
func TestCitationLicenseNotedStreaming(t *testing.T) {
	dec := newStreamDecoder()
	_, err := dec.Feed("", `{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},`+
		`"citationMetadata":{"citationSources":[{"uri":"https://x.example","license":"CC-BY"}]},`+
		`"finishReason":"STOP"}],"usageMetadata":{"candidatesTokenCount":1}}`)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	dec.Finish()
	notes := dec.Notes()
	if !anyNoteContains(notes, r66marker+" 1 citation source") {
		t.Errorf("流式 citation license 没被报出：%v", notes)
	}
}

// 多帧各带 license → 累计计数。
func TestCitationLicenseAccumulateStreaming(t *testing.T) {
	dec := newStreamDecoder()
	_, _ = dec.Feed("", `{"candidates":[{"index":0,"content":{"parts":[{"text":"a"}]},`+
		`"citationMetadata":{"citationSources":[{"uri":"https://a.example","license":"CC-BY"}]}}]}`)
	_, _ = dec.Feed("", `{"candidates":[{"index":0,"content":{"parts":[{"text":"ab"}]},`+
		`"citationMetadata":{"citationSources":[{"uri":"https://b.example","license":"MIT"}]},`+
		`"finishReason":"STOP"}],"usageMetadata":{"candidatesTokenCount":2}}`)
	dec.Finish()
	notes := dec.Notes()
	if !anyNoteContains(notes, r66marker+" 2 citation source") {
		t.Errorf("流式累计 license 计数不对：%v", notes)
	}
}

// 缺席 license → 流式不误报。
func TestCitationLicenseAbsentStaysCleanStreaming(t *testing.T) {
	dec := newStreamDecoder()
	_, _ = dec.Feed("", `{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},`+
		`"citationMetadata":{"citationSources":[{"uri":"https://x.example"}]},"finishReason":"STOP"}]}`)
	dec.Finish()
	notes := dec.Notes()
	if anyNoteContains(notes, r66marker) {
		t.Errorf("缺席 license 流式被误报：%v", notes)
	}
}

// 规则 b：同一份 license 丢弃按 stream / 非流式请求报出的注记逐字一致。
func TestCitationLicenseStreamNonStreamWordingIdentical(t *testing.T) {
	body := `{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},` +
		`"citationMetadata":{"citationSources":[` +
		`{"uri":"https://a.example","license":"CC-BY"},` +
		`{"uri":"https://b.example","license":"MIT"}]},` +
		`"finishReason":"STOP"}],"usageMetadata":{"candidatesTokenCount":1}}`
	_, nsNotes, err := DecodeResponseLossy([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	dec := newStreamDecoder()
	if _, err := dec.Feed("", body); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	dec.Finish()
	sNotes := dec.Notes()

	ns := r66find(nsNotes, r66marker)
	s := r66find(sNotes, r66marker)
	if ns == "" || s == "" {
		t.Fatalf("两路径都应报出 license 注记：非流式=%v 流式=%v", nsNotes, sNotes)
	}
	if ns != s {
		t.Errorf("流式与非流式 license 注记措辞不一致：\n非流式=%q\n流式  =%q", ns, s)
	}
}

// Notes() 取过一次即清零：第二次不再重复 license 注记。
func TestCitationLicenseNoteDrainedOnceStreaming(t *testing.T) {
	dec := newStreamDecoder()
	_, _ = dec.Feed("", `{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},`+
		`"citationMetadata":{"citationSources":[{"uri":"https://x.example","license":"MIT"}]},`+
		`"finishReason":"STOP"}]}`)
	dec.Finish()
	first := dec.Notes()
	if !anyNoteContains(first, r66marker) {
		t.Fatalf("首次 Notes() 应含 license 注记：%v", first)
	}
	second := dec.Notes()
	if anyNoteContains(second, r66marker) {
		t.Errorf("Notes() 未清零，二次仍报 license 注记：%v", second)
	}
}

// 措辞单元核对：license 注记与「无正文块可挂」的引用丢弃注记措辞不同，且点名 license。
func TestCitationLicenseWordingDistinctFromDroppedCitations(t *testing.T) {
	l := codec.CitationLicenseDropNote(1)
	c := codec.DroppedCitationsNote(1)
	if l == c {
		t.Errorf("license 注记不应与 DroppedCitationsNote 逐字相同：%q", l)
	}
	if !strings.Contains(l, "license") {
		t.Errorf("license 注记应点名 license：%q", l)
	}
	if strings.Contains(c, "license") {
		t.Errorf("DroppedCitationsNote 不应含 license：%q", c)
	}
}
