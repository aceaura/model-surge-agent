package gemini

import (
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 流内错误帧没有 HTTP 状态码，只能靠 status 串归类。全归 upstream 会让限流被
// 当成目标故障去累计失败并冷却，而它本该只是等一等再发。
func TestStreamErrorClassifiesByStatus(t *testing.T) {
	cases := []struct {
		status string
		want   ir.ErrorKind
	}{
		{"RESOURCE_EXHAUSTED", ir.ErrRateLimit},
		{"UNAUTHENTICATED", ir.ErrAuth},
		{"PERMISSION_DENIED", ir.ErrAuth},
		{"NOT_FOUND", ir.ErrNotFound},
		{"INVALID_ARGUMENT", ir.ErrInvalidRequest},
		{"UNAVAILABLE", ir.ErrUpstream},
		{"DEADLINE_EXCEEDED", ir.ErrTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			dec := newStreamDecoder()
			events, err := dec.Feed("",
				`{"error":{"status":"`+tc.status+`","message":"boom"}}`)
			if err != nil {
				t.Fatalf("Feed: %v", err)
			}
			if len(events) != 1 || events[0].Type != ir.EvError || events[0].Err == nil {
				t.Fatalf("events = %#v, want one error event", events)
			}
			if got := events[0].Err.Kind; got != tc.want {
				t.Errorf("kind = %q, want %q", got, tc.want)
			}
		})
	}
}

// HTTP 路径与流内路径必须给出同一个分类，否则同一个上游故障会因为
// 走了哪条路而被记成两种结果。
func TestStreamErrorMatchesTheHTTPPath(t *testing.T) {
	body := `{"error":{"status":"RESOURCE_EXHAUSTED","message":"slow down"}}`
	dec := newStreamDecoder()
	events, err := dec.Feed("", body)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	viaHTTP := DecodeError(429, []byte(body))
	if events[0].Err.Kind != viaHTTP.Kind {
		t.Errorf("stream kind = %q, http kind = %q", events[0].Err.Kind, viaHTTP.Kind)
	}
}

// 上游没按本协议的错误结构回：仍要挖出消息，不能只丢一段原始 body。
func TestDecodeErrorExtractsFromForeignShapes(t *testing.T) {
	got := DecodeError(503, []byte(`{"error_msg":"gateway exploded"}`))
	if got.Message != "gateway exploded" {
		t.Errorf("message = %q, want the message dug out of the foreign shape", got.Message)
	}
}

// 上游把下游的整个错误体字符串化塞进 message：真消息在里面。
func TestDecodeErrorUnwrapsJSONStuffedIntoMessage(t *testing.T) {
	body := `{"error":{"status":"INVALID_ARGUMENT",` +
		`"message":"{\"error\":{\"message\":\"prompt is too long\"}}"}}`
	got := DecodeError(400, []byte(body))
	if got.Message != "prompt is too long" {
		t.Errorf("message = %q, the real message is nested inside", got.Message)
	}
	if got.Kind != ir.ErrContextExceeded {
		t.Errorf("kind = %q, want context_exceeded", got.Kind)
	}
}
