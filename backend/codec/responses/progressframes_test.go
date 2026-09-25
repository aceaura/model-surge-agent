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
