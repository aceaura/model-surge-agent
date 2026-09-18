package codec_test

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 各家把消息放在不同字段名下。挖不出来运维就只能读一段原始 body，
// 那等于没做归一化。
func TestExtractMessageWalksTheFallbackChain(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"canonical message", `{"error":{"message":"boom"}}`, "boom"},
		{"top level message", `{"message":"boom"}`, "boom"},
		{"msg", `{"msg":"boom"}`, "boom"},
		{"error_msg", `{"error_msg":"boom"}`, "boom"},
		{"errorMessage", `{"errorMessage":"boom"}`, "boom"},
		{"detail", `{"detail":"boom"}`, "boom"},
		{"description", `{"error":{"description":"boom"}}`, "boom"},
		{"err", `{"err":"boom"}`, "boom"},
		{"reason", `{"reason":"boom"}`, "boom"},
		{"nested under response", `{"response":{"error":{"message":"boom"}}}`, "boom"},
		{"nested under data", `{"data":{"msg":"boom"}}`, "boom"},
		{"list of details", `{"error":{"details":[{"reason":"boom"}]}}`, "boom"},
		// 上游把下游的整个错误体字符串化塞进 message：真消息还在里面，
		// 直接回显运维只能读到一堆转义引号。
		{"json inside message", `{"error":{"message":"{\"error\":{\"message\":\"boom\"}}"}}`, "boom"},
		{"whitespace trimmed", `{"message":"  boom  "}`, "boom"},
		{"not json", `<html>502 Bad Gateway</html>`, ""},
		{"no message anywhere", `{"error":{"type":"api_error"}}`, ""},
		{"empty body", ``, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := codec.ExtractMessage([]byte(tc.body)); got != tc.want {
				t.Errorf("ExtractMessage = %q, want %q", got, tc.want)
			}
		})
	}
}

// message 字段本身就是规范位，但里面可能套着另一层错误体。
func TestRefineMessageUnwrapsNestedJSON(t *testing.T) {
	got := codec.RefineMessage(`{"error":{"message":"prompt is too long"}}`)
	if got != "prompt is too long" {
		t.Errorf("RefineMessage = %q, the real message is nested inside", got)
	}
	if got := codec.RefineMessage("plain words"); got != "plain words" {
		t.Errorf("RefineMessage = %q, a non-JSON message must pass through", got)
	}
}

// 提不到消息也不能产出空消息的错误对象：那在流水里查不出任何东西。
func TestFallbackErrorNeverYieldsAnEmptyMessage(t *testing.T) {
	err := codec.FallbackError(503, []byte(`{"error":{"type":"api_error"}}`))
	if !strings.Contains(err.Message, "503") {
		t.Errorf("message = %q, want the status description as a fallback", err.Message)
	}
	if err.Kind != ir.ErrUpstream {
		t.Errorf("kind = %q, want upstream", err.Kind)
	}
}

// 挖出来的消息要参与归类：上下文超限是 400 里必须单独认出的一类，
// 把它记成目标失败会让一个好目标被冷却。
func TestFallbackErrorClassifiesByTheExtractedMessage(t *testing.T) {
	err := codec.FallbackError(400, []byte(`{"msg":"prompt is too long"}`))
	if err.Kind != ir.ErrContextExceeded {
		t.Errorf("kind = %q, want context_exceeded", err.Kind)
	}
	if err.Message != "prompt is too long" {
		t.Errorf("message = %q", err.Message)
	}
}

// 流内错误帧没有 HTTP 状态码。只看状态码会把一切归成 upstream，
// 于是限流被当作目标故障去累计失败并冷却，而它本该只是等一等再发。
func TestKindForFallsBackToTheBodyCodeWithoutAStatus(t *testing.T) {
	cases := []struct {
		code string
		want ir.ErrorKind
	}{
		{"rate_limit_error", ir.ErrRateLimit},
		{"RESOURCE_EXHAUSTED", ir.ErrRateLimit},
		{"insufficient_quota", ir.ErrRateLimit},
		{"authentication_error", ir.ErrAuth},
		{"permission_denied", ir.ErrAuth},
		{"not_found_error", ir.ErrNotFound},
		{"invalid_request_error", ir.ErrInvalidRequest},
		{"context_length_exceeded", ir.ErrContextExceeded},
		{"overloaded_error", ir.ErrUpstream},
		{"deadline_exceeded", ir.ErrTimeout},
		{"something_nobody_documented", ir.ErrUpstream},
		{"", ir.ErrUpstream},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			if got := codec.KindFor(0, tc.code, ""); got != tc.want {
				t.Errorf("KindFor(0, %q) = %q, want %q", tc.code, got, tc.want)
			}
		})
	}
}

// 有状态码时以它为准：HTTP 层的语义比体内自创的码值可信。
func TestKindForPrefersTheHTTPStatus(t *testing.T) {
	if got := codec.KindFor(429, "api_error", ""); got != ir.ErrRateLimit {
		t.Errorf("kind = %q, the 429 must win over the body code", got)
	}
}

// 参数错误里混着上下文超限，后者不该记作目标的失败。
func TestKindForSeparatesContextOverflowFromInvalidRequest(t *testing.T) {
	got := codec.KindFor(0, "invalid_request_error", "prompt is too long")
	if got != ir.ErrContextExceeded {
		t.Errorf("kind = %q, want context_exceeded", got)
	}
}

// param 指出的是哪个请求字段被拒，是 400 里最有排查价值的一维。
func TestExtractParamReadsBothStandardPositions(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"error.param", `{"error":{"message":"bad","param":"max_tokens"}}`, "max_tokens"},
		{"top-level param", `{"message":"bad","param":"tools[0].name"}`, "tools[0].name"},
		{"error.param wins", `{"param":"outer","error":{"param":"inner"}}`, "inner"},
		{"trims space", `{"error":{"param":"  top_p  "}}`, "top_p"},
		{"absent", `{"error":{"message":"bad"}}`, ""},
		{"null", `{"error":{"param":null}}`, ""},
		{"not a string", `{"error":{"param":{"name":"x"}}}`, ""},
		{"not json", `<html>502</html>`, ""},
		{"json array", `[{"param":"x"}]`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := codec.ExtractParam([]byte(tc.body)); got != tc.want {
				t.Errorf("ExtractParam = %q, want %q", got, tc.want)
			}
		})
	}
}

// 兜底不能覆盖结构化路径已取到的值：后者来自协议自家的字段位，更可信。
func TestWithParamDoesNotOverwrite(t *testing.T) {
	err := ir.NewError(ir.ErrInvalidRequest, 400, "", "bad")
	err.Param = "from_wire"
	codec.WithParam(err, []byte(`{"error":{"param":"from_body"}}`))
	if err.Param != "from_wire" {
		t.Errorf("param = %q, 已有值不该被兜底覆盖", err.Param)
	}
}

func TestWithParamFillsWhenEmpty(t *testing.T) {
	err := ir.NewError(ir.ErrInvalidRequest, 400, "", "bad")
	codec.WithParam(err, []byte(`{"error":{"param":"temperature"}}`))
	if err.Param != "temperature" {
		t.Errorf("param = %q, want temperature", err.Param)
	}
}

// nil 不能让兜底崩：DecodeError 的某些分支理论上可以返回 nil。
func TestWithParamToleratesNil(t *testing.T) {
	if got := codec.WithParam(nil, []byte(`{"error":{"param":"x"}}`)); got != nil {
		t.Errorf("WithParam(nil) = %v, want nil", got)
	}
}
