package responses

import (
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// #76-A1/A2/A3：流式进度帧与未知事件分账注记、logprobs 探测注记、
// usage 细分维度注记。三者同型：内容没有 IR 对应物、只能丢，但丢
// 必须可见——上游新增事件型时，静默忽略等于新能力蒸发且无人知道。

func TestProgressAndUnknownFramesCountedSeparately(t *testing.T) {
	_, notes := feedEvents(t,
		`{"type":"response.created","response":{"id":"r1","model":"m"}}`,
		// 进度信号：queued 与托管调用生命周期帧。
		`{"type":"response.queued"}`,
		`{"type":"response.web_search_call.in_progress","output_index":0}`,
		`{"type":"response.web_search_call.searching","output_index":0}`,
		// 未知事件型。
		`{"type":"response.something_new","payload":1}`,
		`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed"}}`,
	)
	if !anyNoteHas(notes, "3 hosted-call progress frame") {
		t.Errorf("进度帧没按数报出：%v", notes)
	}
	if !anyNoteHas(notes, "1 stream event(s) of a type this decoder does not know") {
		t.Errorf("未知帧没报出：%v", notes)
	}
}

// 已知已处理的事件不许落进计数：created/completed 等再报「未知」
// 就是假警报，会淹没真正的新事件型信号。
func TestHandledEventsNotCounted(t *testing.T) {
	_, notes := feedEvents(t,
		`{"type":"response.created","response":{"id":"r1","model":"m"}}`,
		`{"type":"response.in_progress","response":{"id":"r1","model":"m"}}`,
		`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed"}}`,
	)
	if anyNoteHas(notes, "progress frame") || anyNoteHas(notes, "does not know") {
		t.Errorf("已处理事件被误计数：%v", notes)
	}
}

// output_text part 携带 logprobs：随 output_item.done 的终态快照到达，
// 逐 token 概率没有 IR 槽位，计数报出。
func TestStreamLogprobsPartsCounted(t *testing.T) {
	_, notes := feedEvents(t,
		`{"type":"response.created","response":{"id":"r1","model":"m"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant",`+
			`"content":[{"type":"output_text","text":"hi","logprobs":[{"token":"hi","logprob":-0.1}]}]}}`,
		`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed"}}`,
	)
	if !anyNoteHas(notes, "1 logprobs payload") {
		t.Errorf("流式 logprobs 没被报出：%v", notes)
	}
}

// 非流式整份解码同判据。
func TestWholeDecodeLogprobsNoted(t *testing.T) {
	body := []byte(`{"id":"r1","object":"response","model":"m","status":"completed","output":[` +
		`{"type":"message","role":"assistant","content":[` +
		`{"type":"output_text","text":"hi","logprobs":[{"token":"hi","logprob":-0.1}]},` +
		`{"type":"output_text","text":"there","logprobs":null}]}]}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if !anyNoteHas(notes, "1 logprobs payload") {
		t.Errorf("整份解码 logprobs 没按数报出（null 不算）：%v", notes)
	}
}

// 轮次53：官方把 output_text.logprobs 建模为必填、非空的 LogProb 数组，未请求
// top_logprobs 时其规范值是空数组 []。此前计数条件 `len>0 && !="null"` 会把 [] 也
// 计入，导致每条普通 output_text 响应都误报「dropped N logprobs payload」——违反
// 「注记当且仅当真实丢弃」（误报与漏报同为缺口）。空数组必须静默。流式路径钉住。
func TestStreamEmptyLogprobsArrayNotNoted(t *testing.T) {
	_, notes := feedEvents(t,
		`{"type":"response.created","response":{"id":"r1","model":"m"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant",`+
			`"content":[{"type":"output_text","text":"hi","logprobs":[]}]}}`,
		`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed"}}`,
	)
	if anyNoteHas(notes, "logprobs") {
		t.Errorf("空数组 logprobs:[] 被误报为丢弃：%v", notes)
	}
}

// 空数组带空白（[ ]）同样是合法空数组，不得因字符串比较漏判而误报。
func TestStreamWhitespaceEmptyLogprobsArrayNotNoted(t *testing.T) {
	_, notes := feedEvents(t,
		`{"type":"response.created","response":{"id":"r1","model":"m"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant",`+
			`"content":[{"type":"output_text","text":"hi","logprobs":[ ]}]}}`,
		`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed"}}`,
	)
	if anyNoteHas(notes, "logprobs") {
		t.Errorf("带空白的空数组 logprobs:[ ] 被误报为丢弃：%v", notes)
	}
}

// 非流式整份解码同判据：混合 [] / null / 实载荷，只应数出实载荷那一条。
func TestWholeDecodeEmptyLogprobsArrayNotNoted(t *testing.T) {
	body := []byte(`{"id":"r1","object":"response","model":"m","status":"completed","output":[` +
		`{"type":"message","role":"assistant","content":[` +
		`{"type":"output_text","text":"a","logprobs":[]},` +
		`{"type":"output_text","text":"b","logprobs":null},` +
		`{"type":"output_text","text":"c","logprobs":[{"token":"c","logprob":-0.3}]}]}]}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if !anyNoteHas(notes, "1 logprobs payload") {
		t.Errorf("混合场景应只数出 1 条实载荷（[]/null 不算）：%v", notes)
	}
}

// 全是空数组/null 时，整份解码不得出现任何 logprobs 注记。
func TestWholeDecodeAllEmptyLogprobsSilent(t *testing.T) {
	body := []byte(`{"id":"r1","object":"response","model":"m","status":"completed","output":[` +
		`{"type":"message","role":"assistant","content":[` +
		`{"type":"output_text","text":"a","logprobs":[]},` +
		`{"type":"output_text","text":"b","logprobs":null}]}]}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if anyNoteHas(notes, "logprobs") {
		t.Errorf("全空场景仍误报 logprobs 丢弃：%v", notes)
	}
}

// 直接钉住 hasLogProbsPayload 的分档：空/空白/null/[] → false；实数组、异形非空 → true。
func TestHasLogProbsPayloadClassification(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"", false},
		{"null", false},
		{"  null ", false},
		{"[]", false},
		{"[ ]", false},
		{`[{"token":"a","logprob":-0.1}]`, true},
		{`[null]`, true},                  // 非空数组即算载荷，条目内容交由内容路径负责
		{`{"unexpected":"object"}`, true}, // 异形非数组：无法证明为空，保守报
	}
	for _, c := range cases {
		if got := hasLogProbsPayload([]byte(c.raw)); got != c.want {
			t.Errorf("hasLogProbsPayload(%q) = %v，想要 %v", c.raw, got, c.want)
		}
	}
}

// usage 细分维度：本协议 usage 六位细分都没有槽位，流式与非流式
// 同判据报出。
func TestForeignUsageDimsNoted(t *testing.T) {
	u := ir.Usage{InputTokens: 1, WebSearchRequests: 2, PromptAudioTokens: 3,
		AcceptedPredictionTokens: 4}

	_, notes, err := inboundCodec{}.EncodeResponseLossy(&ir.Response{
		ID: "r1", Model: "m", Usage: u})
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	for _, want := range []string{"web search request count", "prompt audio tokens", "prediction tokens"} {
		if !anyNoteHas(notes, want) {
			t.Errorf("非流式注记缺 %q：%v", want, notes)
		}
	}

	enc := newStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "r1", Model: "m",
		Usage: &u}); err != nil {
		t.Fatalf("Encode(start): %v", err)
	}
	enc.Finish()
	if !anyNoteHas(enc.Notes(), "web search request count") {
		t.Errorf("流式注记缺 usage 细分：%v", enc.Notes())
	}
}

// 细分全零时不报：聚合总量本来就完整，报出来是假警报。
func TestZeroUsageDimsNotNoted(t *testing.T) {
	_, notes, err := inboundCodec{}.EncodeResponseLossy(&ir.Response{
		ID: "r1", Model: "m", Usage: ir.Usage{InputTokens: 1, OutputTokens: 2}})
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if anyNoteHas(notes, "usage detail") {
		t.Errorf("零细分被误报：%v", notes)
	}
}
