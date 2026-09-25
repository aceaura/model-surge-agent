package chatcompletions

import (
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 200 响应体顶层带 error 时必须报错，不得产出 choices 为空的伪造成功。
func TestDecodeResponseLossy_ErrorBodyIn200(t *testing.T) {
	body := []byte(`{"error":{"message":"model is overloaded","type":"server_error","code":529}}`)
	resp, notes, err := DecodeResponseLossy(body)
	if err == nil {
		t.Fatalf("expected error, got response %+v notes %v", resp, notes)
	}
	ire, ok := err.(*ir.Error)
	if !ok {
		t.Fatalf("expected *ir.Error, got %T: %v", err, err)
	}
	if ire.Message != "model is overloaded" {
		t.Fatalf("message not preserved: %q", ire.Message)
	}
}

// 错误归类与流式错误帧同源：rate_limit 的 type 要落到同一 kind。
func TestDecodeResponseLossy_ErrorBodyKind(t *testing.T) {
	body := []byte(`{"error":{"message":"slow down","type":"rate_limit_error"}}`)
	_, _, err := DecodeResponseLossy(body)
	ire, ok := err.(*ir.Error)
	if !ok {
		t.Fatalf("expected *ir.Error, got %T: %v", err, err)
	}
	if ire.Kind != ir.ErrRateLimit {
		t.Fatalf("kind = %v, want ErrRateLimit", ire.Kind)
	}
}

// 正常响应不受 error 检查影响。
func TestDecodeResponseLossy_NormalUnaffected(t *testing.T) {
	body := []byte(`{"id":"c1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	resp, _, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Content) != 1 || resp.Usage.InputTokens != 3 {
		t.Fatalf("bad decode: %+v", resp)
	}
}
