package gemini

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
)

// 轮次69：gemini Candidate.logprobsResult 空壳对象过报修复（假阳性注记）。
//
// 官方 logprobsResult 是对象（topCandidates/chosenCandidates 数组 + logProbabilitySum
// 标量）。此前判据 `len(raw)>0 && string(raw)!="null"` 把空壳对象 `{}` /
// `{"topCandidates":[],"chosenCandidates":[]}`（字段在、无真载荷）也计一次丢弃，
// 误报 LogProbsDropNote——违反「注记当且仅当真实丢弃」（规则 a）。与 responses
// （hasLogProbsPayload）、chat（hasChatLogProbsPayload，轮次68）早已载荷感知形成
// 跨族不对称：gemini 是最后一个仍用朴素存在性判据的族。修法：两路径共用
// hasGeminiLogProbsPayload，仅当真有数组元素或显式 logProbabilitySum 才计。
// 以下钉住流式/非流式两条路径都不再把空壳计入，同时真载荷仍如实报出（不漏）。

const r69fingerprint = "logprobs payload"

// ---- 非流式 ----

// 非流式：logprobsResult 为空对象 {} → 不计数（空壳无真载荷）。
func TestR69EmptyObjectNotCountedNonStreaming(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},` +
		`"logprobsResult":{}}],` +
		`"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if anyNoteContains(notes, r69fingerprint) {
		t.Errorf("空对象 logprobsResult 被误报：%v", notes)
	}
}

// 非流式：logprobsResult 两数组皆空 → 不计数。
func TestR69EmptyArraysNotCountedNonStreaming(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},` +
		`"logprobsResult":{"topCandidates":[],"chosenCandidates":[]}}],` +
		`"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if anyNoteContains(notes, r69fingerprint) {
		t.Errorf("空数组 logprobsResult 被误报：%v", notes)
	}
}

// 非流式：带空白的空对象 `{ }` → 不计数（空白不能绕过判空）。
func TestR69WhitespaceEmptyObjectNotCountedNonStreaming(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},` +
		`"logprobsResult":{ }}],` +
		`"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if anyNoteContains(notes, r69fingerprint) {
		t.Errorf("带空白空对象 logprobsResult 被误报：%v", notes)
	}
}

// 非流式：真载荷（chosenCandidates 非空）→ 仍计数（回归护栏，不因修过报而漏报）。
func TestR69RealPayloadStillCountedNonStreaming(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},` +
		`"logprobsResult":{"chosenCandidates":[{"token":"hi","logProbability":-0.2}]}}],` +
		`"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if !anyNoteContains(notes, "1 "+r69fingerprint) {
		t.Errorf("真载荷 logprobsResult 没被报出：%v", notes)
	}
}

// 非流式：仅带 logProbabilitySum 标量（数组缺席）→ 计数（上游确实算了概率）。
func TestR69SumOnlyCountedNonStreaming(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},` +
		`"logprobsResult":{"logProbabilitySum":-1.5}}],` +
		`"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if !anyNoteContains(notes, "1 "+r69fingerprint) {
		t.Errorf("仅 logProbabilitySum 的真载荷没被报出：%v", notes)
	}
}

// ---- 流式 ----

// 流式：logprobsResult 为空对象 {} → 不计数。
func TestR69EmptyObjectNotCountedStreaming(t *testing.T) {
	dec := newStreamDecoder()
	_, err := dec.Feed("", `{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},`+
		`"logprobsResult":{},"finishReason":"STOP"}]}`)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	dec.Finish()
	if notes := dec.Notes(); anyNoteContains(notes, r69fingerprint) {
		t.Errorf("流式空对象 logprobsResult 被误报：%v", notes)
	}
}

// 流式：logprobsResult 两数组皆空 → 不计数。
func TestR69EmptyArraysNotCountedStreaming(t *testing.T) {
	dec := newStreamDecoder()
	_, _ = dec.Feed("", `{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},`+
		`"logprobsResult":{"topCandidates":[],"chosenCandidates":[]},"finishReason":"STOP"}]}`)
	dec.Finish()
	if notes := dec.Notes(); anyNoteContains(notes, r69fingerprint) {
		t.Errorf("流式空数组 logprobsResult 被误报：%v", notes)
	}
}

// 流式：真载荷 → 仍计数（回归护栏）。
func TestR69RealPayloadStillCountedStreaming(t *testing.T) {
	dec := newStreamDecoder()
	_, _ = dec.Feed("", `{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},`+
		`"logprobsResult":{"chosenCandidates":[{"token":"hi","logProbability":-0.1}]},`+
		`"finishReason":"STOP"}],"usageMetadata":{"candidatesTokenCount":1}}`)
	dec.Finish()
	if notes := dec.Notes(); !anyNoteContains(notes, "1 "+r69fingerprint) {
		t.Errorf("流式真载荷 logprobsResult 没被报出：%v", notes)
	}
}

// 流式：topCandidates 非空（chosenCandidates 缺席）→ 计数（任一数组有载荷即算）。
func TestR69TopCandidatesCountedStreaming(t *testing.T) {
	dec := newStreamDecoder()
	_, _ = dec.Feed("", `{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},`+
		`"logprobsResult":{"topCandidates":[{"candidates":[{"token":"h","logProbability":-0.1}]}]},`+
		`"finishReason":"STOP"}],"usageMetadata":{"candidatesTokenCount":1}}`)
	dec.Finish()
	if notes := dec.Notes(); !anyNoteContains(notes, "1 "+r69fingerprint) {
		t.Errorf("流式 topCandidates 真载荷没被报出：%v", notes)
	}
}

// 规则 b：同一真载荷，流式与非流式措辞一致（共用 LogProbsDropNote）。
func TestR69WordingIdenticalAcrossPaths(t *testing.T) {
	payload := `{"chosenCandidates":[{"token":"hi","logProbability":-0.2}]}`

	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},` +
		`"logprobsResult":` + payload + `}],` +
		`"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}`)
	_, nonStreamNotes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}

	dec := newStreamDecoder()
	_, _ = dec.Feed("", `{"candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]},`+
		`"logprobsResult":`+payload+`,"finishReason":"STOP"}],`+
		`"usageMetadata":{"candidatesTokenCount":1}}`)
	dec.Finish()
	streamNotes := dec.Notes()

	want := codec.LogProbsDropNote(1)
	if !anyNoteContains(nonStreamNotes, want) {
		t.Errorf("非流式措辞不含标准注记 %q：%v", want, nonStreamNotes)
	}
	if !anyNoteContains(streamNotes, want) {
		t.Errorf("流式措辞不含标准注记 %q：%v", want, streamNotes)
	}
}

// ---- 单元：hasGeminiLogProbsPayload 判据表 ----

func TestR69HasGeminiLogProbsPayloadUnit(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"空", "", false},
		{"null 字面量", "null", false},
		{"纯空白", "   ", false},
		{"空对象", "{}", false},
		{"带空白空对象", "{ }", false},
		{"两数组皆空", `{"topCandidates":[],"chosenCandidates":[]}`, false},
		{"仅空 topCandidates", `{"topCandidates":[]}`, false},
		{"仅空 chosenCandidates", `{"chosenCandidates":[]}`, false},
		{"chosenCandidates 有元素", `{"chosenCandidates":[{"token":"a","logProbability":-0.1}]}`, true},
		{"topCandidates 有元素", `{"topCandidates":[{"candidates":[]}]}`, true},
		{"仅 logProbabilitySum", `{"logProbabilitySum":-1.5}`, true},
		{"logProbabilitySum 为零", `{"logProbabilitySum":0}`, true},
		{"异形-数字", "123", true},
		{"异形-字符串", `"garbage"`, true},
		{"异形-数组", "[1,2,3]", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasGeminiLogProbsPayload(json.RawMessage(tc.raw)); got != tc.want {
				t.Errorf("hasGeminiLogProbsPayload(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// 注记去重：多帧各带空壳不累计，多帧各带真载荷才累计。
func TestR69EmptyShellsDoNotAccumulate(t *testing.T) {
	dec := newStreamDecoder()
	_, _ = dec.Feed("", `{"candidates":[{"index":0,"content":{"parts":[{"text":"a"}]},"logprobsResult":{}}]}`)
	_, _ = dec.Feed("", `{"candidates":[{"index":0,"content":{"parts":[{"text":"ab"}]},"logprobsResult":{"chosenCandidates":[]},"finishReason":"STOP"}]}`)
	dec.Finish()
	notes := dec.Notes()
	if anyNoteContains(notes, r69fingerprint) {
		t.Errorf("空壳跨帧被累计误报：%v", notes)
	}
	for _, n := range notes {
		if strings.Contains(n, r69fingerprint) {
			t.Fatalf("不该出现 logprobs 注记：%q", n)
		}
	}
}
