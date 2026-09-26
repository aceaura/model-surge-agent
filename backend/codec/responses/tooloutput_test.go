package responses

import (
	"strings"
	"testing"
)

// 畸形 function_call_output.output（既非合法字符串也非合法 part 数组）此前被
// 静默落成空文本块，把「客户端 output 字段写错了」伪装成「工具返回空串」。
// 现应 400，让客户端知道是它的请求不合法。
func TestMalformedToolCallOutputRejected(t *testing.T) {
	body := `{"model":"m","input":[{"type":"function_call_output","call_id":"c1","output":123}]}`
	_, err := DecodeRequest([]byte(body))
	if err == nil {
		t.Fatalf("畸形 output 应报错，却通过了")
	}
	if !strings.Contains(err.Error(), "function_call_output.output") {
		t.Errorf("错误应指明 output 字段，实得 %v", err)
	}
}

// 合法字符串、空串、缺省都不该报错（空/缺省仍落空文本占位保住 tool 配平）。
func TestValidToolCallOutputAccepted(t *testing.T) {
	for _, out := range []string{`"result text"`, `""`, `[{"type":"output_text","text":"hi"}]`} {
		body := `{"model":"m","input":[{"type":"function_call_output","call_id":"c1","output":` + out + `}]}`
		if _, err := DecodeRequest([]byte(body)); err != nil {
			t.Errorf("output=%s 不该报错: %v", out, err)
		}
	}
	// 缺省 output 字段。
	body := `{"model":"m","input":[{"type":"function_call_output","call_id":"c1"}]}`
	if _, err := DecodeRequest([]byte(body)); err != nil {
		t.Errorf("缺省 output 不该报错: %v", err)
	}
}
