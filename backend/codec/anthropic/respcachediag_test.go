package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次52：anthropic 客户端拿到 responses 上游返回的提示缓存诊断回执
//（ir.Response.ResponsesPromptCacheDiagnostics，流式随 EvMessageDelta / 投影首帧
// EvMessageStart 抵达）时，本协议响应无 prompt_cache_diagnostics 槽位，编码边界
// 整体丢弃——非流式与流式两条路径都必须经有损注记报出，不得静默。
//
// 与轮次49 的 moderation/metadata 注记同款对称防御：anthropic 客户端请求侧本无
// prompt_cache_options 槽位、通常无从索要诊断，但字段一旦非空（上游自发返回或
// 未来路径变化）即照实报，空值绝不误报。措辞与非流式 DescribeResponseClientMetaLoss
// 同源、判据一致。

const cacheDiagFingerprint = "prompt cache diagnostics receipt"

// 非流式：ResponsesPromptCacheDiagnostics 非空 → EncodeResponseLossy 报出。
func TestAnthropicNonStreamCacheDiagNoted(t *testing.T) {
	resp := &ir.Response{ID: "m1", Model: "m", StopReason: ir.StopEndTurn,
		ResponsesPromptCacheDiagnostics: json.RawMessage(`{"type":"cache_hit","cached_tokens":96}`)}
	_, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if !anyNoteHas(notes, cacheDiagFingerprint) {
		t.Fatalf("非流式 anthropic 未报出诊断丢弃：%v", notes)
	}
}

// 非流式：诊断缺席 → 不发明注记。
func TestAnthropicNonStreamCacheDiagAbsentSilent(t *testing.T) {
	resp := &ir.Response{ID: "m1", Model: "m", StopReason: ir.StopEndTurn}
	_, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if anyNoteHas(notes, cacheDiagFingerprint) {
		t.Fatalf("缺席诊断却报了注记：%v", notes)
	}
}

// 流式：诊断随收尾帧 EvMessageDelta 抵达 → Notes() 报出。
func TestAnthropicStreamCacheDiagNoted(t *testing.T) {
	enc := newStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "m1", Model: "m"}); err != nil {
		t.Fatalf("Encode(start): %v", err)
	}
	if _, err := enc.Encode(ir.Event{Type: ir.EvBlockStart, Index: 0,
		Block: &ir.Block{Type: ir.BlockText}}); err != nil {
		t.Fatalf("Encode(blockstart): %v", err)
	}
	if _, err := enc.Encode(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "hi"}); err != nil {
		t.Fatalf("Encode(delta): %v", err)
	}
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageDelta,
		PromptCacheDiagnostics: json.RawMessage(`{"type":"cache_miss"}`)}); err != nil {
		t.Fatalf("Encode(msgdelta): %v", err)
	}
	enc.Finish()
	if !anyNoteHas(enc.Notes(), cacheDiagFingerprint) {
		t.Fatalf("流式 anthropic 未报出诊断丢弃：%v", enc.Notes())
	}
}

// 流式：诊断缺席 → Notes() 不发明注记。
func TestAnthropicStreamCacheDiagAbsentSilent(t *testing.T) {
	enc := newStreamEncoder()
	enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "m1", Model: "m"})
	enc.Encode(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}})
	enc.Encode(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "hi"})
	enc.Encode(ir.Event{Type: ir.EvMessageDelta})
	enc.Finish()
	if anyNoteHas(enc.Notes(), cacheDiagFingerprint) {
		t.Fatalf("缺席诊断却报了注记：%v", enc.Notes())
	}
}

// 流式：Notes() 抽干后二次调用不重复报（去重纪律）。
func TestAnthropicStreamCacheDiagDrainedOnSecondCall(t *testing.T) {
	enc := newStreamEncoder()
	enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "m1", Model: "m"})
	enc.Encode(ir.Event{Type: ir.EvMessageDelta,
		PromptCacheDiagnostics: json.RawMessage(`{"type":"cache_miss"}`)})
	enc.Finish()
	if !anyNoteHas(enc.Notes(), cacheDiagFingerprint) {
		t.Fatalf("首次 Notes() 未报出：%v", enc.Notes())
	}
	if anyNoteHas(enc.Notes(), cacheDiagFingerprint) {
		t.Fatalf("二次 Notes() 重复报出（未抽干）：%v", enc.Notes())
	}
}
