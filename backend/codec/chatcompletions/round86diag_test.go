package chatcompletions

import (
	"encoding/json"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次86：anthropic 响应级请求诊断回执（ir.Response.AnthropicDiagnostics，官方
// Message.diagnostics={cache_miss_reason}）跨族投给 chat 客户端时，本协议响应没有
// diagnostics 槽位，编码边界整体丢弃——非流式与流式两条路径都必须经有损注记报出。
// 与 respcachediag_test.go（那是 responses 族的 prompt_cache_diagnostics 被 chat 丢）
// 是两个不同族、不同线格式的回执，各自独立报。措辞与非流式 codec.go 同源、判据一致。
//
// 对称防御：实践中 chat 客户端无从索要 anthropic 诊断（请求侧无对应槽位、丢弃另由
// describeRequestLossy 报出），字段几乎不可达；但一旦非空即照实报，空/显式 null 绝不误报。

const respDiagFingerprint = "request-level diagnostics receipt"

func respDiagPayload() json.RawMessage {
	return json.RawMessage(`{"cache_miss_reason":{"type":"cache_miss_model_changed"}}`)
}

// 非流式：AnthropicDiagnostics 非空 → EncodeResponseLossy 报出。
func TestChatNonStreamRespDiagNoted(t *testing.T) {
	resp := &ir.Response{ID: "c1", Model: "m", StopReason: ir.StopEndTurn,
		AnthropicDiagnostics: respDiagPayload()}
	_, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if !anyNoteHas(notes, respDiagFingerprint) {
		t.Fatalf("非流式 chat 未报出 anthropic 诊断丢弃：%v", notes)
	}
}

// 非流式：缺席 → 不发明注记。
func TestChatNonStreamRespDiagAbsentSilent(t *testing.T) {
	resp := &ir.Response{ID: "c1", Model: "m", StopReason: ir.StopEndTurn}
	_, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if anyNoteHas(notes, respDiagFingerprint) {
		t.Fatalf("缺席诊断却报了注记：%v", notes)
	}
}

// 非流式：显式 null 不算回执，不报（与解码侧归一口径一致）。
func TestChatNonStreamRespDiagNullSilent(t *testing.T) {
	resp := &ir.Response{ID: "c1", Model: "m", StopReason: ir.StopEndTurn,
		AnthropicDiagnostics: json.RawMessage(`null`)}
	_, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if anyNoteHas(notes, respDiagFingerprint) {
		t.Fatalf("显式 null 诊断被误报：%v", notes)
	}
}

// 流式：诊断随首帧 EvMessageStart 抵达（官方只在完整 Message 上带它）→ Notes() 报出。
func TestChatStreamRespDiagNoted(t *testing.T) {
	enc := newStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "c1", Model: "m",
		AnthropicDiagnostics: respDiagPayload()}); err != nil {
		t.Fatalf("Encode(start): %v", err)
	}
	if _, err := enc.Encode(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "hi"}); err != nil {
		t.Fatalf("Encode(delta): %v", err)
	}
	enc.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn})
	enc.Finish()
	if !anyNoteHas(enc.Notes(), respDiagFingerprint) {
		t.Fatalf("流式 chat 未报出 anthropic 诊断丢弃：%v", enc.Notes())
	}
}

// 流式整份响应投影路径（上游忽略 stream:true）：ResponseEvents 把诊断投到首帧
// EvMessageStart，编码器收下、Notes() 报出。
func TestChatStreamRespDiagFromReplay(t *testing.T) {
	resp := &ir.Response{ID: "c1", Model: "m", StopReason: ir.StopEndTurn,
		AnthropicDiagnostics: respDiagPayload()}
	enc := newStreamEncoder()
	for _, ev := range ir.ResponseEvents(resp) {
		if _, err := enc.Encode(ev); err != nil {
			t.Fatalf("Encode: %v", err)
		}
	}
	enc.Finish()
	if !anyNoteHas(enc.Notes(), respDiagFingerprint) {
		t.Fatalf("投影路径流式 chat 未报出 anthropic 诊断丢弃：%v", enc.Notes())
	}
}

// 流式：缺席 → Notes() 不发明注记。
func TestChatStreamRespDiagAbsentSilent(t *testing.T) {
	enc := newStreamEncoder()
	enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "c1", Model: "m"})
	enc.Encode(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "hi"})
	enc.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn})
	enc.Finish()
	if anyNoteHas(enc.Notes(), respDiagFingerprint) {
		t.Fatalf("缺席诊断却报了注记：%v", enc.Notes())
	}
}

// 流式：Notes() 抽干后二次调用不重复报（去重纪律，与非流式一致）。
func TestChatStreamRespDiagDrainedOnSecondCall(t *testing.T) {
	enc := newStreamEncoder()
	enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "c1", Model: "m",
		AnthropicDiagnostics: respDiagPayload()})
	enc.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn})
	enc.Finish()
	if !anyNoteHas(enc.Notes(), respDiagFingerprint) {
		t.Fatalf("首次 Notes() 未报出：%v", enc.Notes())
	}
	if anyNoteHas(enc.Notes(), respDiagFingerprint) {
		t.Fatalf("二次 Notes() 重复报出（未抽干）：%v", enc.Notes())
	}
}
