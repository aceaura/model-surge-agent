package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次86：anthropic 响应级请求诊断回执（官方 Message.diagnostics={cache_miss_reason}）
// 的同族保全。这是轮次85 请求侧 diagnostics.previous_message_id 的回执半边：客户端
// 索要后，上游在完整 Message 上回填「prompt-cache 前缀为何未能复用」的归因。此前
// wireResponse / streamMsg 都没有 diagnostics 槽位，DecodeResponse 与 message_start
// 解码都不读它——连同族 anthropic→anthropic 往返都静默丢弃（违规则 a/c）。
//
// 官方形状（anthropic-sdk-python）：message.py:65 Message.diagnostics:Optional[
// Diagnostics]；diagnostics.py Diagnostics.cache_miss_reason:Optional[CacheMissReason]；
// cache_miss_reason.py 是判别式联合（model_changed/system_changed/tools_changed/
// messages_changed/previous_message_not_found/unavailable）。流式只在 message_start
// 的完整 Message 上抵达——raw_message_delta_event.py 的 Delta 无 diagnostics 字段。
// 跨族丢弃注记见 codec/{chatcompletions,responses}/round86diag_test.go。

const respDiagBody = `{"id":"m1","type":"message","role":"assistant","model":"m",` +
	`"content":[],"usage":{"input_tokens":1,"output_tokens":1},` +
	`"diagnostics":{"cache_miss_reason":{"type":"cache_miss_model_changed"}}}`

// 非流式解码：diagnostics 原文落进 IR.AnthropicDiagnostics。
func TestRespDiagnosticsDecode(t *testing.T) {
	resp, err := DecodeResponse([]byte(respDiagBody))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(resp.AnthropicDiagnostics) == 0 {
		t.Fatal("diagnostics 未落进 IR")
	}
	if !strings.Contains(string(resp.AnthropicDiagnostics), "cache_miss_model_changed") {
		t.Errorf("diagnostics 原文漂移：%s", resp.AnthropicDiagnostics)
	}
	var probe any
	if err := json.Unmarshal(resp.AnthropicDiagnostics, &probe); err != nil {
		t.Errorf("diagnostics 非合法 JSON：%v", err)
	}
}

// 非流式解码：缺席与显式 null 都归一为空（不把 4 字节字面量当有效回执）。
func TestRespDiagnosticsDecodeAbsentAndNull(t *testing.T) {
	for _, body := range []string{
		`{"id":"m1","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`,
		`{"id":"m1","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":1,"output_tokens":1},"diagnostics":null}`,
	} {
		resp, err := DecodeResponse([]byte(body))
		if err != nil {
			t.Fatalf("DecodeResponse: %v", err)
		}
		if len(resp.AnthropicDiagnostics) != 0 {
			t.Errorf("缺席/null 时 diagnostics 应为空：%s", resp.AnthropicDiagnostics)
		}
	}
}

// 非流式解码：{cache_miss_reason:null}（已索要、后台比对未完成）外层对象非空，收下。
func TestRespDiagnosticsDecodePendingNullInner(t *testing.T) {
	body := `{"id":"m1","type":"message","role":"assistant","model":"m","content":[],` +
		`"usage":{"input_tokens":1,"output_tokens":1},"diagnostics":{"cache_miss_reason":null}}`
	resp, err := DecodeResponse([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if !strings.Contains(string(resp.AnthropicDiagnostics), "cache_miss_reason") {
		t.Errorf("pending 回执（内层 null）应原样收下：%s", resp.AnthropicDiagnostics)
	}
}

// 同族非流式往返：decode→encode 逐字回写 diagnostics；同族不报有损注记。
func TestRespDiagnosticsSameFamilyRoundTrip(t *testing.T) {
	resp, err := DecodeResponse([]byte(respDiagBody))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	out, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if !strings.Contains(string(out), `"diagnostics"`) || !strings.Contains(string(out), "cache_miss_model_changed") {
		t.Errorf("同族未逐字回写 diagnostics：%s", out)
	}
	// 同族保全是无损的，绝不允许对自己报「丢弃 diagnostics」。
	for _, n := range notes {
		if strings.Contains(n, "diagnostics") {
			t.Errorf("同族 anthropic 误报 diagnostics 有损：%v", notes)
		}
	}
	// 回写后再解码仍稳定（不漂移）。
	back, err := DecodeResponse(out)
	if err != nil {
		t.Fatalf("DecodeResponse(往返): %v", err)
	}
	if !strings.Contains(string(back.AnthropicDiagnostics), "cache_miss_model_changed") {
		t.Errorf("往返 diagnostics 漂移：%s", back.AnthropicDiagnostics)
	}
}

// 同族非流式：缺席时不出 diagnostics 键。
func TestRespDiagnosticsEncodeAbsentNoKey(t *testing.T) {
	out, err := EncodeResponse(&ir.Response{ID: "m1", Model: "m", StopReason: ir.StopEndTurn})
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if strings.Contains(string(out), "diagnostics") {
		t.Errorf("缺席不应出 diagnostics 键：%s", out)
	}
}

// 流式解码：message_start 的完整 Message 上携带 diagnostics → 落进 EvMessageStart 事件。
func TestRespDiagnosticsStreamDecode(t *testing.T) {
	dec := newStreamDecoder()
	data := `{"type":"message_start","message":{"id":"m1","type":"message","role":"assistant",` +
		`"model":"m","content":[],"stop_reason":null,"stop_sequence":null,` +
		`"usage":{"input_tokens":1,"output_tokens":0},` +
		`"diagnostics":{"cache_miss_reason":{"type":"cache_miss_system_changed"}}}}`
	evs, err := dec.Feed("message_start", data)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(evs) == 0 || evs[0].Type != ir.EvMessageStart {
		t.Fatalf("未解出 message_start 事件：%+v", evs)
	}
	if !strings.Contains(string(evs[0].AnthropicDiagnostics), "cache_miss_system_changed") {
		t.Errorf("流式 message_start 的 diagnostics 未落进事件：%s", evs[0].AnthropicDiagnostics)
	}
}

// 流式编码：EvMessageStart 带 diagnostics → message_start 帧逐字回写（同族保全）。
func TestRespDiagnosticsStreamEncode(t *testing.T) {
	enc := newStreamEncoder()
	frames, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "m1", Model: "m",
		AnthropicDiagnostics: json.RawMessage(`{"cache_miss_reason":{"type":"cache_miss_tools_changed"}}`)})
	if err != nil {
		t.Fatalf("Encode(start): %v", err)
	}
	joined := string(frames[0])
	for _, f := range frames[1:] {
		joined += string(f)
	}
	if !strings.Contains(joined, `"diagnostics"`) || !strings.Contains(joined, "cache_miss_tools_changed") {
		t.Errorf("流式 message_start 未回写 diagnostics：%s", joined)
	}
	// 同族流式保全无损，不得报丢弃。
	enc.Finish()
	for _, n := range enc.Notes() {
		if strings.Contains(n, "diagnostics") {
			t.Errorf("同族 anthropic 流式误报 diagnostics 有损：%v", n)
		}
	}
}

// 流式整份响应投影路径（上游忽略 stream:true）：ResponseEvents 把 diagnostics 投到
// 首帧 EvMessageStart，同族编码器逐字回写（钉住 replay.go 的投影）。
func TestRespDiagnosticsStreamFromReplay(t *testing.T) {
	resp, err := DecodeResponse([]byte(respDiagBody))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	enc := newStreamEncoder()
	var joined string
	for _, ev := range ir.ResponseEvents(resp) {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		for _, f := range frames {
			joined += string(f)
		}
	}
	enc.Finish()
	if !strings.Contains(joined, "cache_miss_model_changed") {
		t.Errorf("投影路径流式未回写 diagnostics：%s", joined)
	}
}

// 聚合往返：message_start 事件带 diagnostics → Aggregator 重建的 Response 收下它
// （钉住 aggregate.go 首帧合并；官方 Delta 无此字段，故只在首帧分支合并）。
func TestRespDiagnosticsAggregate(t *testing.T) {
	var a ir.Aggregator
	a.Add(ir.Event{Type: ir.EvMessageStart, MessageID: "m1", Model: "m",
		AnthropicDiagnostics: json.RawMessage(`{"cache_miss_reason":{"type":"cache_miss_unavailable"}}`)})
	a.Add(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn})
	got := a.Response()
	if !strings.Contains(string(got.AnthropicDiagnostics), "cache_miss_unavailable") {
		t.Errorf("聚合后 diagnostics 丢失：%s", got.AnthropicDiagnostics)
	}
}
