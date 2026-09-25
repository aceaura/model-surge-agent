package responses

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 非流式响应的终态失败不得伪造成 completed——此前只判 incomplete，
// failed/cancelled 会产出 200+空 output 的"成功"，error 全丢，
// 与流式路径遇到 failed 帧的口径相反。
func TestDecodeResponseFailedStatusNotDisguised(t *testing.T) {
	body := `{"id":"r1","status":"failed","output":[],` +
		`"error":{"code":"server_error","message":"model exploded"}}`
	_, err := DecodeResponse([]byte(body))
	if err == nil {
		t.Fatal("status=failed 被伪造成成功响应")
	}
	ie, ok := err.(*ir.Error)
	if !ok {
		t.Fatalf("want *ir.Error, got %T", err)
	}
	if !strings.Contains(ie.Message, "model exploded") {
		t.Fatalf("错误详情丢失: %q", ie.Message)
	}
}

func TestDecodeResponseCancelledStatusNotDisguised(t *testing.T) {
	_, err := DecodeResponse([]byte(`{"id":"r1","status":"cancelled","output":[]}`))
	if err == nil {
		t.Fatal("status=cancelled 被伪造成成功响应")
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("错误里应带上状态: %v", err)
	}
}

// 非终态（queued/in_progress）不是成功也不是终态失败：
// 解下去会得到内容残缺的「正常」响应，按可重试上游错误处理。
func TestDecodeResponseNonTerminalStatusRetryable(t *testing.T) {
	for _, status := range []string{"queued", "in_progress"} {
		_, err := DecodeResponse([]byte(`{"id":"r1","status":"` + status + `","output":[]}`))
		if err == nil {
			t.Fatalf("status=%s 被当成成功响应", status)
		}
		ie, ok := err.(*ir.Error)
		if !ok {
			t.Fatalf("want *ir.Error, got %T", err)
		}
		if !ie.Retryable {
			t.Fatalf("status=%s 应可重试", status)
		}
		if !strings.Contains(ie.Message, status) {
			t.Fatalf("错误里应带上状态: %q", ie.Message)
		}
	}
}

// completed/incomplete 不受影响，照常解码。
func TestDecodeResponseTerminalStatusesPassThrough(t *testing.T) {
	resp, err := DecodeResponse([]byte(`{"id":"r1","status":"completed","output":[` +
		`{"type":"message","content":[{"type":"output_text","text":"hi"}]}]}`))
	if err != nil {
		t.Fatalf("completed: %v", err)
	}
	if resp.StopReason != ir.StopEndTurn {
		t.Fatalf("completed stop reason = %q", resp.StopReason)
	}

	resp, err = DecodeResponse([]byte(`{"id":"r1","status":"incomplete","output":[]}`))
	if err != nil {
		t.Fatalf("incomplete: %v", err)
	}
	if resp.StopReason != ir.StopMaxTokens {
		t.Fatalf("incomplete stop reason = %q", resp.StopReason)
	}
}
