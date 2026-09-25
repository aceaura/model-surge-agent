package anthropic

import (
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// #76-A3：usage 细分维度的跨族损耗注记。本协议的原生细分是托管工具
// 执行次数与推理区域（#70 已双向建模）；chat 专属的音频/预测四位没有
// 槽位，转进本协议会蒸发——聚合总量不丢，细分丢了要报出，流式与
// 非流式同判据（codec.UsageDropDims）。

func TestForeignUsageDimsNoted(t *testing.T) {
	u := ir.Usage{InputTokens: 1, OutputTokens: 2,
		PromptAudioTokens: 5, CompletionAudioTokens: 6,
		AcceptedPredictionTokens: 7}

	// 非流式。
	_, notes, err := inboundCodec{}.EncodeResponseLossy(&ir.Response{
		ID: "msg_1", Model: "m", Usage: u})
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if !hasNote(notes, "prompt audio tokens") || !hasNote(notes, "prediction tokens") {
		t.Errorf("非流式注记缺音频/预测细分：%v", notes)
	}

	// 流式：同一判据，不许两条路径口径分叉。
	enc := newStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "msg_1",
		Model: "m", Usage: &u}); err != nil {
		t.Fatalf("Encode(start): %v", err)
	}
	enc.Finish()
	if !hasNote(enc.Notes(), "prompt audio tokens") {
		t.Errorf("流式注记缺音频细分：%v", enc.Notes())
	}
}

// 本族原生维度（托管执行次数/推理区域）不报：#70 之后它们有槽位且
// 已随 usage 写出，报出来就是假警报。
func TestNativeUsageDimsNotNoted(t *testing.T) {
	_, notes, err := inboundCodec{}.EncodeResponseLossy(&ir.Response{
		ID: "msg_1", Model: "m",
		Usage: ir.Usage{WebSearchRequests: 2, WebFetchRequests: 1, InferenceGeo: "us"}})
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if hasNote(notes, "usage detail") {
		t.Errorf("本族原生维度被误报：%v", notes)
	}

	enc := newStreamEncoder()
	u := ir.Usage{WebSearchRequests: 2, InferenceGeo: "us"}
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "msg_1",
		Model: "m", Usage: &u}); err != nil {
		t.Fatalf("Encode(start): %v", err)
	}
	enc.Finish()
	if hasNote(enc.Notes(), "usage detail") {
		t.Errorf("流式误报原生维度：%v", enc.Notes())
	}
}
