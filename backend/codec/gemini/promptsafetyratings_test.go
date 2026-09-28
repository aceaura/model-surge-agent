package gemini

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次65：gemini 上游 promptFeedback.safetyRatings（官方 PromptFeedback.safetyRatings，
// 「Ratings for safety of the prompt」）评的是**用户输入本身**，与轮次50 已覆盖的
// Candidate.safetyRatings（评模型输出）同维异源。此前 wireFeedback 只建模 blockReason，
// 整段 prompt 级评级被 json.Unmarshal 静默吞掉：prompt 被安全拦截（candidates 为空）时
// 客户端只拿到一个 blockReason 枚举，看不到命中哪些危害类别/概率/是否拦截。
//
// 以下钉住三条不变量：
//   - 规则 a（不漏报）：流式与非流式两条路径都探测计数并经 PromptSafetyRatingsDropNote 报出；
//   - 规则 a（不过报）：缺席/空数组/只有 blockReason 时不误报；计数不以 blockReason 为前提
//     （未拦截的信息性评级同样报）；
//   - 规则 a（措辞匹配处置）+ 规则 b（流式/非流式同损同措辞）：prompt 级用 prompt 专属措辞，
//     与候选级分账、可同帧并存互不覆盖；两路径逐字一致。
//
// 官方 blockReasonMessage 字段不在 v1beta discovery 文档里（未证实存在），故不建模、不测。

const (
	r65promptMarker = "per-category safety rating(s) from the upstream prompt feedback"
	r65candMarker   = "per-category safety rating(s) from the upstream candidate"
	r65blockMarker  = "block reason:"
)

// r65find 返回 notes 里第一条含 marker 的注记原文，找不到返回空串。
func r65find(notes []string, marker string) string {
	for _, n := range notes {
		if strings.Contains(n, marker) {
			return n
		}
	}
	return ""
}

// —— 非流式 ——

// prompt 被拦截（blockReason + safetyRatings，candidates 为空）→ 停因、阻断原文、
// prompt 级评级注记三者俱在。
func TestPromptSafetyRatingsNotedNonStreaming(t *testing.T) {
	body := []byte(`{"promptFeedback":{"blockReason":"PROHIBITED_CONTENT",` +
		`"safetyRatings":[{"category":"HARM_CATEGORY_DANGEROUS_CONTENT","probability":"HIGH","blocked":true}]}}`)
	resp, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if resp.StopReason != ir.StopContentFilter {
		t.Errorf("阻断 prompt 的停因应是 content_filter，实为 %q", resp.StopReason)
	}
	if !anyNoteContains(notes, r65blockMarker) {
		t.Errorf("阻断原文串没被带出：%v", notes)
	}
	if !anyNoteContains(notes, "1 "+r65promptMarker) {
		t.Errorf("非流式 prompt 级 safetyRatings 没被报出：%v", notes)
	}
}

// 多条 prompt 级 safetyRatings → 按条数累计计数。
func TestPromptSafetyRatingsCountedNonStreaming(t *testing.T) {
	body := []byte(`{"promptFeedback":{"blockReason":"SAFETY","safetyRatings":[` +
		`{"category":"HARM_CATEGORY_HATE_SPEECH","probability":"LOW"},` +
		`{"category":"HARM_CATEGORY_DANGEROUS_CONTENT","probability":"MEDIUM","blocked":true}` +
		`]}}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if !anyNoteContains(notes, "2 "+r65promptMarker) {
		t.Errorf("非流式多条 prompt 级 safetyRatings 计数不对：%v", notes)
	}
}

// promptFeedback.safetyRatings 存在但无 blockReason（未拦截的信息性评级）→ 仍报出，
// 且不设 content_filter 停因、不发阻断原文注记。钉住「计数不以 blockReason 为前提」。
func TestPromptSafetyRatingsInformationalNonStreaming(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}],` +
		`"promptFeedback":{"safetyRatings":[{"category":"HARM_CATEGORY_HATE_SPEECH","probability":"LOW"}]},` +
		`"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}`)
	resp, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if !anyNoteContains(notes, "1 "+r65promptMarker) {
		t.Errorf("未拦截的信息性 prompt 级评级没被报出：%v", notes)
	}
	if anyNoteContains(notes, r65blockMarker) {
		t.Errorf("无 blockReason 却发了阻断原文注记（过报）：%v", notes)
	}
	if resp.StopReason == ir.StopContentFilter {
		t.Errorf("未拦截却被判成 content_filter 停因：%q", resp.StopReason)
	}
}

// promptFeedback.safetyRatings 为空数组（blockReason 仍在）→ 阻断原文报、prompt 级评级不报。
func TestPromptSafetyRatingsEmptyNotCountedNonStreaming(t *testing.T) {
	body := []byte(`{"promptFeedback":{"blockReason":"SAFETY","safetyRatings":[]}}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if !anyNoteContains(notes, r65blockMarker) {
		t.Errorf("阻断原文串没被带出：%v", notes)
	}
	if anyNoteContains(notes, r65promptMarker) {
		t.Errorf("空 prompt 级 safetyRatings 被误报：%v", notes)
	}
}

// promptFeedback 只有 blockReason、无 safetyRatings 键 → 阻断原文报、prompt 级评级不报。
func TestPromptBlockReasonOnlyNoSafetyNoteNonStreaming(t *testing.T) {
	body := []byte(`{"promptFeedback":{"blockReason":"RECITATION"}}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if !anyNoteContains(notes, r65blockMarker) {
		t.Errorf("阻断原文串没被带出：%v", notes)
	}
	if anyNoteContains(notes, r65promptMarker) {
		t.Errorf("缺席 prompt 级 safetyRatings 被误报：%v", notes)
	}
}

// 完全没有 promptFeedback → 干净，不误报。
func TestPromptSafetyRatingsAbsentStaysCleanNonStreaming(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if anyNoteContains(notes, r65promptMarker) {
		t.Errorf("缺席 promptFeedback 被误报 prompt 级评级：%v", notes)
	}
}

// prompt 级与候选级 safetyRatings 并存 → 两条措辞各异的注记同在、互不覆盖。
// 钉住规则 a「措辞匹配处置」：prompt 级不复用候选级措辞。
func TestPromptAndCandidateSafetyRatingsDistinctNonStreaming(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},` +
		`"safetyRatings":[{"category":"HARM_CATEGORY_HATE_SPEECH","probability":"LOW"}],"finishReason":"STOP"}],` +
		`"promptFeedback":{"safetyRatings":[{"category":"HARM_CATEGORY_DANGEROUS_CONTENT","probability":"MEDIUM"}]},` +
		`"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	pn := r65find(notes, r65promptMarker)
	cn := r65find(notes, r65candMarker)
	if pn == "" {
		t.Errorf("prompt 级 safetyRatings 注记缺失：%v", notes)
	}
	if cn == "" {
		t.Errorf("候选级 safetyRatings 注记缺失：%v", notes)
	}
	if pn != "" && pn == cn {
		t.Errorf("prompt 级与候选级注记措辞不应相同：%q", pn)
	}
}

// —— 流式 ——

// prompt 被拦截帧 → Notes() 报出 prompt 级评级注记。
func TestPromptSafetyRatingsNotedStreaming(t *testing.T) {
	dec := newStreamDecoder()
	_, err := dec.Feed("", `{"promptFeedback":{"blockReason":"PROHIBITED_CONTENT",`+
		`"safetyRatings":[{"category":"HARM_CATEGORY_DANGEROUS_CONTENT","probability":"HIGH","blocked":true}]}}`)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	dec.Finish()
	notes := dec.Notes()
	if !anyNoteContains(notes, "1 "+r65promptMarker) {
		t.Errorf("流式 prompt 级 safetyRatings 没被报出：%v", notes)
	}
}

// 多帧各带 promptFeedback.safetyRatings → 累计计数。
func TestPromptSafetyRatingsAccumulateStreaming(t *testing.T) {
	dec := newStreamDecoder()
	_, _ = dec.Feed("", `{"promptFeedback":{"safetyRatings":[{"category":"HARM_CATEGORY_HATE_SPEECH","probability":"LOW"}]}}`)
	_, _ = dec.Feed("", `{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}],`+
		`"promptFeedback":{"safetyRatings":[{"category":"HARM_CATEGORY_DANGEROUS_CONTENT","probability":"MEDIUM","blocked":true}]},`+
		`"usageMetadata":{"candidatesTokenCount":1}}`)
	dec.Finish()
	notes := dec.Notes()
	if !anyNoteContains(notes, "2 "+r65promptMarker) {
		t.Errorf("流式累计 prompt 级 safetyRatings 计数不对：%v", notes)
	}
}

// 未拦截的信息性 prompt 级评级（无 blockReason）→ 流式仍报出。
func TestPromptSafetyRatingsInformationalStreaming(t *testing.T) {
	dec := newStreamDecoder()
	_, _ = dec.Feed("", `{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}],`+
		`"promptFeedback":{"safetyRatings":[{"category":"HARM_CATEGORY_HATE_SPEECH","probability":"LOW"}]}}`)
	dec.Finish()
	notes := dec.Notes()
	if !anyNoteContains(notes, "1 "+r65promptMarker) {
		t.Errorf("流式未拦截的信息性 prompt 级评级没被报出：%v", notes)
	}
	if anyNoteContains(notes, r65blockMarker) {
		t.Errorf("无 blockReason 却发了阻断原文注记（过报）：%v", notes)
	}
}

// 缺席 promptFeedback → 流式不误报。
func TestPromptSafetyRatingsAbsentStaysCleanStreaming(t *testing.T) {
	dec := newStreamDecoder()
	_, _ = dec.Feed("", `{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}]}`)
	dec.Finish()
	notes := dec.Notes()
	if anyNoteContains(notes, r65promptMarker) {
		t.Errorf("缺席 promptFeedback 流式被误报：%v", notes)
	}
}

// 规则 b：同一份 prompt 级评级按 stream / 非流式请求报出的注记逐字一致。
func TestPromptSafetyRatingsStreamNonStreamWordingIdentical(t *testing.T) {
	body := `{"promptFeedback":{"blockReason":"SAFETY","safetyRatings":[` +
		`{"category":"HARM_CATEGORY_HATE_SPEECH","probability":"LOW"},` +
		`{"category":"HARM_CATEGORY_DANGEROUS_CONTENT","probability":"MEDIUM","blocked":true}` +
		`]}}`
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

	ns := r65find(nsNotes, r65promptMarker)
	s := r65find(sNotes, r65promptMarker)
	if ns == "" || s == "" {
		t.Fatalf("两路径都应报出 prompt 级注记：非流式=%v 流式=%v", nsNotes, sNotes)
	}
	if ns != s {
		t.Errorf("流式与非流式 prompt 级注记措辞不一致：\n非流式=%q\n流式  =%q", ns, s)
	}
}

// Notes() 取过一次即清零：第二次不再重复 prompt 级注记。
func TestPromptSafetyRatingsNoteDrainedOnceStreaming(t *testing.T) {
	dec := newStreamDecoder()
	_, _ = dec.Feed("", `{"promptFeedback":{"blockReason":"SAFETY",`+
		`"safetyRatings":[{"category":"HARM_CATEGORY_HATE_SPEECH","probability":"LOW"}]}}`)
	dec.Finish()
	first := dec.Notes()
	if !anyNoteContains(first, r65promptMarker) {
		t.Fatalf("首次 Notes() 应含 prompt 级注记：%v", first)
	}
	second := dec.Notes()
	if anyNoteContains(second, r65promptMarker) {
		t.Errorf("Notes() 未清零，二次仍报 prompt 级注记：%v", second)
	}
}

// 措辞单元核对：prompt 级与候选级注记原文不同，且 prompt 级点名「prompt feedback」。
func TestPromptSafetyRatingsWordingDistinctFromCandidate(t *testing.T) {
	p := codec.PromptSafetyRatingsDropNote(1)
	c := codec.SafetyRatingsDropNote(1)
	if p == c {
		t.Errorf("prompt 级与候选级注记不应逐字相同：%q", p)
	}
	if !strings.Contains(p, "prompt feedback") {
		t.Errorf("prompt 级注记应点名 prompt feedback：%q", p)
	}
	if strings.Contains(c, "prompt feedback") {
		t.Errorf("候选级注记不应含 prompt feedback：%q", c)
	}
}
