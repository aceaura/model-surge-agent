package gemini

import (
	"testing"
)

// gemini 上游对候选算出的按类别内容安全评级放在 Candidate.safetyRatings
// （Output only，[]SafetyRating{category, probability, blocked}）。IR 响应模型
// 没有结构化安全评级槽位，此前 wireCandidate 未建模该键→json.Unmarshal 静默
// 吞掉、无计数无注记，与同为 Output only 的 logprobsResult / citationMetadata /
// groundingMetadata / urlContextMetadata 都已建模并报出的纪律不对称。
// 以下钉住：流式与非流式两条路径都探测存在性并计数报出（SafetyRatingsDropNote）。

// 非流式：候选带 safetyRatings → 注记报出。
func TestSafetyRatingsNotedNonStreaming(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},` +
		`"safetyRatings":[{"category":"HARM_CATEGORY_HATE_SPEECH","probability":"LOW","blocked":false}],` +
		`"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if !anyNoteContains(notes, "1 per-category safety rating") {
		t.Errorf("非流式 safetyRatings 没被报出：%v", notes)
	}
}

// 非流式：多条 safetyRatings → 按条数累计计数。
func TestSafetyRatingsCountedNonStreaming(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},` +
		`"safetyRatings":[` +
		`{"category":"HARM_CATEGORY_HATE_SPEECH","probability":"LOW"},` +
		`{"category":"HARM_CATEGORY_DANGEROUS_CONTENT","probability":"MEDIUM","blocked":true}` +
		`],"finishReason":"SAFETY"}],` +
		`"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if !anyNoteContains(notes, "2 per-category safety rating") {
		t.Errorf("非流式多条 safetyRatings 计数不对：%v", notes)
	}
}

// 非流式：safetyRatings 为空数组 → 不计数（len==0）。
func TestSafetyRatingsEmptyNotCountedNonStreaming(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},` +
		`"safetyRatings":[],"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if anyNoteContains(notes, "safety rating") {
		t.Errorf("空 safetyRatings 被误报：%v", notes)
	}
}

// 非流式：候选不带 safetyRatings → 不计数。
func TestSafetyRatingsAbsentStaysCleanNonStreaming(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},` +
		`"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if anyNoteContains(notes, "safety rating") {
		t.Errorf("缺席 safetyRatings 被误报：%v", notes)
	}
}

// 非流式：多候选（index>0 被丢）的 safetyRatings 不计数（只处理 index=0）。
func TestSafetyRatingsExtraCandidateNotCounted(t *testing.T) {
	body := []byte(`{"candidates":[` +
		`{"index":0,"content":{"parts":[{"text":"a"}]},"finishReason":"STOP"},` +
		`{"index":1,"content":{"parts":[{"text":"b"}]},` +
		`"safetyRatings":[{"category":"HARM_CATEGORY_HATE_SPEECH","probability":"LOW"}]}` +
		`],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2}}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if anyNoteContains(notes, "safety rating") {
		t.Errorf("extra candidate 的 safetyRatings 不该计入：%v", notes)
	}
}

// 流式：候选带 safetyRatings → Notes() 报出。
func TestSafetyRatingsNotedStreaming(t *testing.T) {
	dec := newStreamDecoder()
	_, err := dec.Feed("", `{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},`+
		`"safetyRatings":[{"category":"HARM_CATEGORY_HATE_SPEECH","probability":"LOW"}],`+
		`"finishReason":"STOP"}],"usageMetadata":{"candidatesTokenCount":1}}`)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	dec.Finish()
	notes := dec.Notes()
	if !anyNoteContains(notes, "1 per-category safety rating") {
		t.Errorf("流式 safetyRatings 没被报出：%v", notes)
	}
}

// 流式：多帧各带 safetyRatings → 累计计数。
func TestSafetyRatingsAccumulateAcrossFrames(t *testing.T) {
	dec := newStreamDecoder()
	_, _ = dec.Feed("", `{"candidates":[{"index":0,"content":{"parts":[{"text":"a"}]},`+
		`"safetyRatings":[{"category":"HARM_CATEGORY_HATE_SPEECH","probability":"LOW"}]}]}`)
	_, _ = dec.Feed("", `{"candidates":[{"index":0,"content":{"parts":[{"text":"ab"}]},`+
		`"safetyRatings":[{"category":"HARM_CATEGORY_DANGEROUS_CONTENT","probability":"MEDIUM","blocked":true}],`+
		`"finishReason":"STOP"}],"usageMetadata":{"candidatesTokenCount":2}}`)
	dec.Finish()
	notes := dec.Notes()
	if !anyNoteContains(notes, "2 per-category safety rating") {
		t.Errorf("累计 safetyRatings 计数不对：%v", notes)
	}
}

// 流式：safetyRatings 缺席 → 不计数。
func TestSafetyRatingsAbsentStaysCleanStreaming(t *testing.T) {
	dec := newStreamDecoder()
	_, _ = dec.Feed("", `{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},`+
		`"finishReason":"STOP"}]}`)
	dec.Finish()
	notes := dec.Notes()
	if anyNoteContains(notes, "safety rating") {
		t.Errorf("缺席 safetyRatings 流式被误报：%v", notes)
	}
}
