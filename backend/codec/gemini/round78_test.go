package gemini

import (
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次78：gemini 上游可能以 HTTP 200 返回 in-band 的 {"error":{...}}（错误装在体内
// 而非状态码上），此时 candidates 往往为空。流式路径对同一 wireResponse 类型早有守卫
// （feedOne 的 frame.Error 分支 → EvError），chat / responses 的非流式解码同样
// 守卫（chatcompletions/decode_stream.go:559-565、responses/decode_stream.go:917-924），
// 唯独 gemini 非流式 DecodeResponseLossy 从不读 w.Error（wire.go:183 已建模）——照解
// 下去产出一份零内容的伪造「成功」，调用方据此记账并报告正常结束，上游错误整段静默
// 蒸发。违规则 b（流式报、非流式静默）与规则 c（跨族同类同报）。修复=补齐守卫，
// 归类与流式同源 convertError(w.Error.Code, w.Error)。

// 非流式：200 带 in-band error → 返回 *ir.Error，绝不产出伪造成功。
func TestNonStreamInBandErrorIsNotFabricatedSuccess(t *testing.T) {
	body := []byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"slow down"}}`)
	resp, _, err := DecodeResponseLossy(body)
	if err == nil {
		t.Fatalf("in-band error 被当成成功：resp=%+v（应为 *ir.Error）", resp)
	}
	if resp != nil {
		t.Errorf("出错时不应返回响应体：%+v", resp)
	}
	ie, ok := err.(*ir.Error)
	if !ok {
		t.Fatalf("错误类型 = %T，want *ir.Error", err)
	}
	if ie.Kind != ir.ErrRateLimit {
		t.Errorf("归类 = %q，want %q（限流被当成目标故障会去冷却）", ie.Kind, ir.ErrRateLimit)
	}
	if ie.Message != "slow down" {
		t.Errorf("消息 = %q，want slow down——上游归因不能丢", ie.Message)
	}
}

// 规则 b：同一 in-band error 体，流式与非流式必须给出同一归类与消息。
func TestNonStreamInBandErrorMatchesStreaming(t *testing.T) {
	body := `{"error":{"status":"RESOURCE_EXHAUSTED","message":"slow down"}}`
	dec := newStreamDecoder()
	events, ferr := dec.Feed("", body)
	if ferr != nil {
		t.Fatalf("Feed: %v", ferr)
	}
	if len(events) != 1 || events[0].Type != ir.EvError || events[0].Err == nil {
		t.Fatalf("流式应产出一个错误事件：%#v", events)
	}
	_, _, nerr := DecodeResponseLossy([]byte(body))
	if nerr == nil {
		t.Fatal("非流式漏报 in-band error")
	}
	ie, ok := nerr.(*ir.Error)
	if !ok {
		t.Fatalf("非流式错误类型 = %T，want *ir.Error", nerr)
	}
	if ie.Kind != events[0].Err.Kind {
		t.Errorf("归类不一致：stream=%q nonstream=%q", events[0].Err.Kind, ie.Kind)
	}
	if ie.Message != events[0].Err.Message {
		t.Errorf("消息不一致：stream=%q nonstream=%q", events[0].Err.Message, ie.Message)
	}
}

// 回归守卫：正常响应（无 error 键）仍照常解码，不被新守卫误伤。
func TestNonStreamNormalResponseUnaffectedByErrorGuard(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"ok"}]}}],` +
		`"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5}}`)
	resp, _, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("正常响应被误判为错误：%v", err)
	}
	if resp == nil || len(resp.Content) != 1 {
		t.Fatalf("正常响应应解出一个文本块：%+v", resp)
	}
}
