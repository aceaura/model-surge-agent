package responses

import (
	"encoding/json"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次86：anthropic 响应级请求诊断回执（ir.Response.AnthropicDiagnostics，官方
// Message.diagnostics={cache_miss_reason}）跨族投给 responses 客户端时整体丢弃——
// 本协议虽有 prompt_cache_diagnostics，但那是 responses 族自己的机制（不同线格式：
// {type,cached_tokens}），承载不了 anthropic 的 cache_miss_reason 判别式联合，两者
// 互不投影。非流式与流式两条路径都必须经有损注记报出，措辞与非流式 codec.go 同源。
//
// 对称防御：responses 客户端无从索要 anthropic 诊断，字段几乎不可达；一旦非空即照实
// 报，空/显式 null 绝不误报。注意与本协议同族保全的 ResponsesPromptCacheDiagnostics
// 区分——那个是 responses→responses 原样带回、不报。

const respDiagFingerprint = "request-level diagnostics receipt"

func respDiagPayload() json.RawMessage {
	return json.RawMessage(`{"cache_miss_reason":{"type":"cache_miss_messages_changed"}}`)
}

// 非流式：AnthropicDiagnostics 非空 → EncodeResponseLossy 报出。
func TestResponsesNonStreamRespDiagNoted(t *testing.T) {
	resp := &ir.Response{ID: "r1", Model: "m", StopReason: ir.StopEndTurn,
		AnthropicDiagnostics: respDiagPayload()}
	_, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if !anyNoteHas(notes, respDiagFingerprint) {
		t.Fatalf("非流式 responses 未报出 anthropic 诊断丢弃：%v", notes)
	}
}

// 非流式：缺席 → 不发明注记。
func TestResponsesNonStreamRespDiagAbsentSilent(t *testing.T) {
	resp := &ir.Response{ID: "r1", Model: "m", StopReason: ir.StopEndTurn}
	_, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if anyNoteHas(notes, respDiagFingerprint) {
		t.Fatalf("缺席诊断却报了注记：%v", notes)
	}
}

// 非流式：显式 null 不算回执，不报。
func TestResponsesNonStreamRespDiagNullSilent(t *testing.T) {
	resp := &ir.Response{ID: "r1", Model: "m", StopReason: ir.StopEndTurn,
		AnthropicDiagnostics: json.RawMessage(`null`)}
	_, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if anyNoteHas(notes, respDiagFingerprint) {
		t.Fatalf("显式 null 诊断被误报：%v", notes)
	}
}

// 非流式：同族的 prompt_cache_diagnostics 仍原样保全、不误报 anthropic 诊断丢弃。
func TestResponsesNonStreamOwnCacheDiagUnaffected(t *testing.T) {
	resp := &ir.Response{ID: "r1", Model: "m", StopReason: ir.StopEndTurn,
		ResponsesPromptCacheDiagnostics: json.RawMessage(`{"type":"cache_hit","cached_tokens":64}`)}
	_, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if anyNoteHas(notes, respDiagFingerprint) {
		t.Fatalf("responses 同族 prompt_cache_diagnostics 触发了 anthropic 诊断注记：%v", notes)
	}
}

// 流式：诊断随首帧 EvMessageStart 抵达 → Notes() 报出。
func TestResponsesStreamRespDiagNoted(t *testing.T) {
	enc := newStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "r1", Model: "m",
		AnthropicDiagnostics: respDiagPayload()}); err != nil {
		t.Fatalf("Encode(start): %v", err)
	}
	if _, err := enc.Encode(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "hi"}); err != nil {
		t.Fatalf("Encode(delta): %v", err)
	}
	enc.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn})
	enc.Finish()
	if !anyNoteHas(enc.Notes(), respDiagFingerprint) {
		t.Fatalf("流式 responses 未报出 anthropic 诊断丢弃：%v", enc.Notes())
	}
}

// 流式整份响应投影路径：ResponseEvents 把诊断投到首帧 EvMessageStart，Notes() 报出。
func TestResponsesStreamRespDiagFromReplay(t *testing.T) {
	resp := &ir.Response{ID: "r1", Model: "m", StopReason: ir.StopEndTurn,
		AnthropicDiagnostics: respDiagPayload()}
	enc := newStreamEncoder()
	for _, ev := range ir.ResponseEvents(resp) {
		if _, err := enc.Encode(ev); err != nil {
			t.Fatalf("Encode: %v", err)
		}
	}
	enc.Finish()
	if !anyNoteHas(enc.Notes(), respDiagFingerprint) {
		t.Fatalf("投影路径流式 responses 未报出 anthropic 诊断丢弃：%v", enc.Notes())
	}
}

// 流式：缺席 → Notes() 不发明注记。
func TestResponsesStreamRespDiagAbsentSilent(t *testing.T) {
	enc := newStreamEncoder()
	enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "r1", Model: "m"})
	enc.Encode(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "hi"})
	enc.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn})
	enc.Finish()
	if anyNoteHas(enc.Notes(), respDiagFingerprint) {
		t.Fatalf("缺席诊断却报了注记：%v", enc.Notes())
	}
}
