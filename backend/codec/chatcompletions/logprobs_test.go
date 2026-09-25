package chatcompletions

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// #76-A2/A3：响应侧 logprobs 探测注记与 usage 细分维度注记。
// logprobs 请求侧开关可贯通，算出来的逐 token 概率却没有 IR 槽位——
// 静默丢掉时客户端开了 logprobs=true 也永远收不到数据，还以为上游没算。
// usage 细分同理：anthropic 专属的托管执行次数/推理区域转进本协议会蒸发。

func TestStreamLogprobsCountedInNotes(t *testing.T) {
	dec := newStreamDecoder()
	frames := []string{
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"logprobs":{"content":[{"token":"hi","logprob":-0.1}]}}]}`,
		doneSentinel,
	}
	for _, f := range frames {
		if _, err := dec.Feed("", f); err != nil {
			t.Fatalf("feed %s: %v", f, err)
		}
	}
	if !anyNoteHas(dec.Notes(), "1 logprobs payload") {
		t.Errorf("logprobs 载荷没被报出：%v", dec.Notes())
	}
}

// 显式 null 与缺省同义，不算载荷。
func TestStreamNullLogprobsNotCounted(t *testing.T) {
	dec := newStreamDecoder()
	frames := []string{
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"hi"},"logprobs":null}]}`,
		doneSentinel,
	}
	for _, f := range frames {
		if _, err := dec.Feed("", f); err != nil {
			t.Fatalf("feed %s: %v", f, err)
		}
	}
	if anyNoteHas(dec.Notes(), "logprobs") {
		t.Errorf("null logprobs 被误报：%v", dec.Notes())
	}
}

func TestWholeResponseLogprobsNoted(t *testing.T) {
	body := []byte(`{"id":"c1","object":"chat.completion","created":1700000000,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop",` +
		`"logprobs":{"content":[{"token":"done","logprob":-0.2}]}}]}`)
	_, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if !anyNoteHas(notes, "1 logprobs payload") {
		t.Errorf("非流式 logprobs 没被报出：%v", notes)
	}
}

// usage 细分：anthropic 专属维度转进本协议要报出，流式与非流式同判据。
func TestForeignUsageDimsNoted(t *testing.T) {
	// 非流式。
	_, notes, err := inboundCodec{}.EncodeResponseLossy(&ir.Response{ID: "c1", Model: "m",
		Usage: ir.Usage{InputTokens: 1, WebSearchRequests: 2, InferenceGeo: "us"}})
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if !anyNoteHas(notes, "web search request count") || !anyNoteHas(notes, "inference geo") {
		t.Errorf("非流式 usage 细分损耗没报全：%v", notes)
	}

	// 流式：同一判据（UsageDropDims），不许两条路径口径分叉。
	enc := newStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "c1", Model: "m",
		Usage: &ir.Usage{InputTokens: 1, WebSearchRequests: 2, InferenceGeo: "us"}}); err != nil {
		t.Fatalf("Encode(start): %v", err)
	}
	enc.Finish()
	if !anyNoteHas(enc.Notes(), "web search request count") {
		t.Errorf("流式 usage 细分损耗没报出：%v", enc.Notes())
	}
}

// 本族原生维度（音频/预测）不报：它们有槽位且已随 usage 写出，
// 报出来就是假警报。
func TestNativeUsageDimsNotNoted(t *testing.T) {
	_, notes, err := inboundCodec{}.EncodeResponseLossy(&ir.Response{ID: "c1", Model: "m",
		Usage: ir.Usage{PromptAudioTokens: 5, CompletionAudioTokens: 6,
			AcceptedPredictionTokens: 7, RejectedPredictionTokens: 8}})
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if anyNoteHas(notes, "usage detail") {
		t.Errorf("本族原生维度被误报：%v", notes)
	}
	// 且原值确实写出了。
	out, err := EncodeResponse(&ir.Response{ID: "c1", Model: "m",
		Usage: ir.Usage{PromptAudioTokens: 5, AcceptedPredictionTokens: 7}})
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if !strings.Contains(string(out), `"audio_tokens":5`) ||
		!strings.Contains(string(out), `"accepted_prediction_tokens":7`) {
		t.Errorf("原生维度没写出：%s", out)
	}
}
