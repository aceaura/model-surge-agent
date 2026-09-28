package chatcompletions

import (
	"encoding/json"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次52：chat 客户端经本网关路由到 responses 上游、并用请求侧
// prompt_cache_options.comparison_response_id 索要了缓存诊断时，上游回的诊断回执
// 落进 ir.Response.ResponsesPromptCacheDiagnostics（流式则随 EvMessageDelta /
// 投影首帧 EvMessageStart 抵达）。官方 ChatCompletion 响应无 prompt_cache_diagnostics
// 槽位，编码边界整体丢弃——非流式与流式两条路径都必须经有损注记报出，不得静默。
// 措辞与非流式 DescribeResponseClientMetaLoss 同源（判据一致）。
//
// 承载维 moderation / metadata 不在本轮口径：chat 对那两维是承载族、原样带回不报。

const cacheDiagFingerprint = "prompt cache diagnostics receipt"

// 非流式：ResponsesPromptCacheDiagnostics 非空 → EncodeResponseLossy 报出。
func TestChatNonStreamCacheDiagNoted(t *testing.T) {
	resp := &ir.Response{ID: "c1", Model: "m", StopReason: ir.StopEndTurn,
		ResponsesPromptCacheDiagnostics: json.RawMessage(`{"type":"cache_miss"}`)}
	_, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if !anyNoteHas(notes, cacheDiagFingerprint) {
		t.Fatalf("非流式 chat 未报出诊断丢弃：%v", notes)
	}
}

// 非流式：诊断缺席 → 不发明注记。
func TestChatNonStreamCacheDiagAbsentSilent(t *testing.T) {
	resp := &ir.Response{ID: "c1", Model: "m", StopReason: ir.StopEndTurn}
	_, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if anyNoteHas(notes, cacheDiagFingerprint) {
		t.Fatalf("缺席诊断却报了注记：%v", notes)
	}
}

// 非流式：显式 null 不算回执，不报（与解码侧归一口径一致）。
func TestChatNonStreamCacheDiagNullSilent(t *testing.T) {
	resp := &ir.Response{ID: "c1", Model: "m", StopReason: ir.StopEndTurn,
		ResponsesPromptCacheDiagnostics: json.RawMessage(`null`)}
	_, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if anyNoteHas(notes, cacheDiagFingerprint) {
		t.Fatalf("显式 null 诊断被误报：%v", notes)
	}
}

// 流式：诊断随收尾帧 EvMessageDelta 抵达 → Notes() 报出。
func TestChatStreamCacheDiagNoted(t *testing.T) {
	enc := newStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "c1", Model: "m"}); err != nil {
		t.Fatalf("Encode(start): %v", err)
	}
	if _, err := enc.Encode(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "hi"}); err != nil {
		t.Fatalf("Encode(delta): %v", err)
	}
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageDelta,
		PromptCacheDiagnostics: json.RawMessage(`{"type":"cache_hit","cached_tokens":64}`)}); err != nil {
		t.Fatalf("Encode(msgdelta): %v", err)
	}
	enc.Finish()
	if !anyNoteHas(enc.Notes(), cacheDiagFingerprint) {
		t.Fatalf("流式 chat 未报出诊断丢弃：%v", enc.Notes())
	}
}

// 流式整份响应投影路径（上游忽略 stream:true）：ResponseEvents 把诊断投到首帧
// EvMessageStart，编码器收下、Notes() 报出。
func TestChatStreamCacheDiagFromReplay(t *testing.T) {
	resp := &ir.Response{ID: "c1", Model: "m", StopReason: ir.StopEndTurn,
		ResponsesPromptCacheDiagnostics: json.RawMessage(`{"type":"cache_miss"}`)}
	enc := newStreamEncoder()
	for _, ev := range ir.ResponseEvents(resp) {
		if _, err := enc.Encode(ev); err != nil {
			t.Fatalf("Encode: %v", err)
		}
	}
	enc.Finish()
	if !anyNoteHas(enc.Notes(), cacheDiagFingerprint) {
		t.Fatalf("投影路径流式 chat 未报出诊断丢弃：%v", enc.Notes())
	}
}

// 流式：诊断缺席 → Notes() 不发明注记。
func TestChatStreamCacheDiagAbsentSilent(t *testing.T) {
	enc := newStreamEncoder()
	enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "c1", Model: "m"})
	enc.Encode(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "hi"})
	enc.Encode(ir.Event{Type: ir.EvMessageDelta})
	enc.Finish()
	if anyNoteHas(enc.Notes(), cacheDiagFingerprint) {
		t.Fatalf("缺席诊断却报了注记：%v", enc.Notes())
	}
}

// 流式：Notes() 抽干后二次调用不重复报（去重纪律，与非流式一致）。
func TestChatStreamCacheDiagDrainedOnSecondCall(t *testing.T) {
	enc := newStreamEncoder()
	enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "c1", Model: "m"})
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
