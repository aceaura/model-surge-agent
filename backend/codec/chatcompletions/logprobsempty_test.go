package chatcompletions

import (
	"encoding/json"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
)

// 轮次68：chat_completions 逐 token 概率注记 LogProbsDropNote **过报**（假阳性，
// 违反规则 a：假阳性与漏报同样是缺口）。此前流式/非流式判据都是
// `len(choice.LogProbs)>0 && string(choice.LogProbs)!="null"`，只要 logprobs 是个
// 非 null 的对象就计一次。但官方 choice.logprobs = anyOf[{content:array|null,
// refusal:array|null（二者 required）}, null]：无内容 token 的 choice（纯 tool_call、
// 或只带 finish_reason 的空 delta 收尾帧）会给出 content/refusal 皆为 null 或空数组的
// 空壳对象，这类空壳没有任何逐 token 概率却被计入 → 每条空壳误报。与 responses
// hasLogProbsPayload（专门防空数组过报）不对称。修法：payload-aware 判据
// hasChatLogProbsPayload，两路径共用，仅当 content 或 refusal 是非空数组才计。

// 空壳对象（content/refusal 皆 null）流式不计数。
func TestStreamEmptyLogprobsObjectNotCounted(t *testing.T) {
	dec := newStreamDecoder()
	frames := []string{
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"hi"},"logprobs":{"content":null,"refusal":null}}]}`,
		doneSentinel,
	}
	for _, f := range frames {
		if _, err := dec.Feed("", f); err != nil {
			t.Fatalf("feed %s: %v", f, err)
		}
	}
	if anyNoteHas(dec.Notes(), "logprobs") {
		t.Errorf("空壳 logprobs 对象被误报：%v", dec.Notes())
	}
}

// 空数组形状（content:[] / refusal:[]）流式不计数。
func TestStreamEmptyArrayLogprobsNotCounted(t *testing.T) {
	dec := newStreamDecoder()
	frames := []string{
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"hi"},"logprobs":{"content":[],"refusal":[]}}]}`,
		doneSentinel,
	}
	for _, f := range frames {
		if _, err := dec.Feed("", f); err != nil {
			t.Fatalf("feed %s: %v", f, err)
		}
	}
	if anyNoteHas(dec.Notes(), "logprobs") {
		t.Errorf("空数组 logprobs 被误报：%v", dec.Notes())
	}
}

// 回归：真载荷（content 非空）流式仍计数。
func TestStreamRealLogprobsStillCounted(t *testing.T) {
	dec := newStreamDecoder()
	frames := []string{
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"hi"},"logprobs":{"content":[{"token":"hi","logprob":-0.1}],"refusal":null}}]}`,
		doneSentinel,
	}
	for _, f := range frames {
		if _, err := dec.Feed("", f); err != nil {
			t.Fatalf("feed %s: %v", f, err)
		}
	}
	if !anyNoteHas(dec.Notes(), codec.LogProbsDropNote(1)) {
		t.Errorf("真 logprobs 载荷没被报出：%v", dec.Notes())
	}
}

// refusal 侧真载荷（content 为 null 但 refusal 非空）流式仍计数。
func TestStreamRefusalLogprobsCounted(t *testing.T) {
	dec := newStreamDecoder()
	frames := []string{
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"no"},"logprobs":{"content":null,"refusal":[{"token":"no","logprob":-0.5}]}}]}`,
		doneSentinel,
	}
	for _, f := range frames {
		if _, err := dec.Feed("", f); err != nil {
			t.Fatalf("feed %s: %v", f, err)
		}
	}
	if !anyNoteHas(dec.Notes(), codec.LogProbsDropNote(1)) {
		t.Errorf("refusal logprobs 载荷没被报出：%v", dec.Notes())
	}
}

// 非流式：空壳对象不计数。
func TestWholeResponseEmptyLogprobsObjectNotCounted(t *testing.T) {
	body := []byte(`{"id":"c1","object":"chat.completion","created":1700000000,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop",` +
		`"logprobs":{"content":null,"refusal":null}}]}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if anyNoteHas(notes, "logprobs") {
		t.Errorf("非流式空壳 logprobs 被误报：%v", notes)
	}
}

// 非流式：空对象 {} 不计数。
func TestWholeResponseBareObjectLogprobsNotCounted(t *testing.T) {
	body := []byte(`{"id":"c1","object":"chat.completion","created":1700000000,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop",` +
		`"logprobs":{}}]}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if anyNoteHas(notes, "logprobs") {
		t.Errorf("非流式空对象 logprobs 被误报：%v", notes)
	}
}

// 非流式回归：真载荷仍计数。
func TestWholeResponseRealLogprobsStillCounted(t *testing.T) {
	body := []byte(`{"id":"c1","object":"chat.completion","created":1700000000,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop",` +
		`"logprobs":{"content":[{"token":"done","logprob":-0.2}],"refusal":null}}]}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if !anyNoteHas(notes, codec.LogProbsDropNote(1)) {
		t.Errorf("非流式真 logprobs 载荷没被报出：%v", notes)
	}
}

// 判据单元测试：畸形/异形 logprobs 保守按存在计（真丢弃宁报勿漏），空壳/null/缺省不计。
func TestHasChatLogProbsPayloadUnit(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"empty raw", ``, false},
		{"null literal", `null`, false},
		{"whitespace", `   `, false},
		{"bare object", `{}`, false},
		{"content+refusal null", `{"content":null,"refusal":null}`, false},
		{"content+refusal empty arrays", `{"content":[],"refusal":[]}`, false},
		{"content empty refusal null", `{"content":[],"refusal":null}`, false},
		{"content non-empty", `{"content":[{"token":"a","logprob":-0.1}],"refusal":null}`, true},
		{"refusal non-empty", `{"content":null,"refusal":[{"token":"a","logprob":-0.1}]}`, true},
		{"both non-empty", `{"content":[{"token":"a"}],"refusal":[{"token":"b"}]}`, true},
		{"malformed number", `123`, true},
		{"malformed string", `"garbage"`, true},
		{"malformed array", `[1,2,3]`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := hasChatLogProbsPayload(json.RawMessage(c.raw))
			if got != c.want {
				t.Errorf("hasChatLogProbsPayload(%s) = %v, want %v", c.raw, got, c.want)
			}
		})
	}
}
