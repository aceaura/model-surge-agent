package gemini

import (
	"strings"
	"testing"
)

// gemini 上游按 responseLogprobs=true 计算逐 token 对数概率后放在
// Candidate.logprobsResult（Output only，LogprobsResult{topCandidates,
// chosenCandidates, logProbabilitySum}）。IR 响应模型没有逐 token 概率槽位，
// 此前 wireCandidate 未建模该键→json.Unmarshal 静默吞掉、无计数无注记，
// 与 chat/responses 解码器都按 LogProbsDropNote 报出不对称。
// 以下钉住：流式与非流式两条路径都探测存在性并计数报出。

// 非流式：候选带 logprobsResult → 注记报出。
func TestLogprobsNotedNonStreaming(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},` +
		`"logprobsResult":{"chosenCandidates":[{"token":"hi","logProbability":-0.2}]}}],` +
		`"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if !anyNoteContains(notes, "1 logprobs payload") {
		t.Errorf("非流式 logprobsResult 没被报出：%v", notes)
	}
}

// 非流式：logprobsResult 为 null → 不计数（与 chat/responses 同款判据）。
func TestLogprobsNullNotCountedNonStreaming(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},` +
		`"logprobsResult":null}],` +
		`"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if anyNoteContains(notes, "logprobs") {
		t.Errorf("null logprobsResult 被误报：%v", notes)
	}
}

// 非流式：候选不带 logprobsResult → 不计数。
func TestLogprobsAbsentStaysCleanNonStreaming(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]}}],` +
		`"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if anyNoteContains(notes, "logprobs") {
		t.Errorf("缺席 logprobsResult 被误报：%v", notes)
	}
}

// 流式：候选带 logprobsResult → Notes() 报出。
func TestLogprobsNotedStreaming(t *testing.T) {
	dec := newStreamDecoder()
	_, err := dec.Feed("", `{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},`+
		`"logprobsResult":{"chosenCandidates":[{"token":"hi","logProbability":-0.1}]},`+
		`"finishReason":"STOP"}],"usageMetadata":{"candidatesTokenCount":1}}`)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	dec.Finish()
	notes := dec.Notes()
	if !anyNoteContains(notes, "1 logprobs payload") {
		t.Errorf("流式 logprobsResult 没被报出：%v", notes)
	}
}

// 流式：多帧各带 logprobsResult → 累计计数。
func TestLogprobsAccumulateAcrossFrames(t *testing.T) {
	dec := newStreamDecoder()
	_, _ = dec.Feed("", `{"candidates":[{"index":0,"content":{"parts":[{"text":"a"}]},`+
		`"logprobsResult":{"chosenCandidates":[{"token":"a","logProbability":-0.1}]}}]}`)
	_, _ = dec.Feed("", `{"candidates":[{"index":0,"content":{"parts":[{"text":"ab"}]},`+
		`"logprobsResult":{"chosenCandidates":[{"token":"b","logProbability":-0.2}]},"finishReason":"STOP"}],`+
		`"usageMetadata":{"candidatesTokenCount":2}}`)
	dec.Finish()
	notes := dec.Notes()
	if !anyNoteContains(notes, "2 logprobs payload") {
		t.Errorf("累计 logprobs 计数不对：%v", notes)
	}
}

// 流式：logprobsResult 为 null → 不计数。
func TestLogprobsNullNotCountedStreaming(t *testing.T) {
	dec := newStreamDecoder()
	_, _ = dec.Feed("", `{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},`+
		`"logprobsResult":null,"finishReason":"STOP"}]}`)
	dec.Finish()
	notes := dec.Notes()
	if anyNoteContains(notes, "logprobs") {
		t.Errorf("null logprobsResult 流式被误报：%v", notes)
	}
}

// 非流式：多候选（index>0 被丢）的 logprobsResult 不计数（只处理 index=0）。
func TestLogprobsExtraCandidateNotCounted(t *testing.T) {
	body := []byte(`{"candidates":[` +
		`{"index":0,"content":{"parts":[{"text":"a"}]}},` +
		`{"index":1,"content":{"parts":[{"text":"b"}]},"logprobsResult":{"chosenCandidates":[]}}` +
		`],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2}}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if anyNoteContains(notes, "logprobs") {
		t.Errorf("extra candidate 的 logprobs 不该计入：%v", notes)
	}
}

func anyNoteContains(notes []string, sub string) bool {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}
