package codec_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// rateLimitBodies 是各协议的 429 错误体。
//
// 每个协议一份而不用同一份：DecodeError 在「认得本协议的错误结构」与
// 「回落到 FallbackError」两条路径上分别要填 RetryAfter，用一份通用体
// 只会走其中一条。
var rateLimitBodies = map[string]string{
	codec.ProtocolAnthropic: `{"type":"error","error":{"type":"rate_limit_error",` +
		`"message":"rate limit exceeded"}}`,
	codec.ProtocolChatCompletions: `{"error":{"type":"rate_limit_exceeded",` +
		`"message":"rate limit exceeded"}}`,
	codec.ProtocolResponses: `{"error":{"type":"rate_limit_exceeded",` +
		`"message":"rate limit exceeded"}}`,
	codec.ProtocolGemini: `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED",` +
		`"message":"Quota exceeded"}}`,
}

func rateLimitBody(t *testing.T, name string) []byte {
	t.Helper()
	body, ok := rateLimitBodies[name]
	if !ok {
		t.Fatalf("outbound %q has no rate-limit fixture", name)
	}
	return []byte(body)
}

// TestEveryOutboundReadsRetryAfterHeader 四协议都必须读到限流头。
//
// 限流头是 HTTP 层的，任何出站都可能收到；漏掉一个就意味着那个目标
// 永远按默认时长猜，而漏掉这件事在运行时看不出来。
func TestEveryOutboundReadsRetryAfterHeader(t *testing.T) {
	for _, name := range outboundNames() {
		t.Run(name, func(t *testing.T) {
			c, _ := codec.Outbound(name)
			h := http.Header{}
			h.Set("Retry-After", "300")

			before := time.Now()
			err := c.DecodeError(429, h, rateLimitBody(t, name))
			after := time.Now()

			if err.RetryAfter.IsZero() {
				t.Fatalf("RetryAfter 为零：限流头没被读到")
			}
			// 断言落在区间而非精确值：出口用的是 time.Now()。
			lo, hi := before.Add(300*time.Second), after.Add(300*time.Second)
			if err.RetryAfter.Before(lo) || err.RetryAfter.After(hi) {
				t.Fatalf("RetryAfter = %v，应落在 [%v, %v]", err.RetryAfter, lo, hi)
			}
		})
	}
}

// TestFallbackPathAlsoReadsRetryAfterHeader 回落路径同样要读头。
//
// 与上一条是两条不同的代码路径：网关回 HTML 或自创字段名时走
// FallbackError，那一条也必须套上出口。真实限流常发生在兼容层网关上，
// 而它们的错误体最不规范。
func TestFallbackPathAlsoReadsRetryAfterHeader(t *testing.T) {
	for _, name := range outboundNames() {
		t.Run(name, func(t *testing.T) {
			c, _ := codec.Outbound(name)
			h := http.Header{}
			h.Set("Retry-After", "60")

			err := c.DecodeError(429, h, []byte(`<html>429 Too Many Requests</html>`))
			if err.RetryAfter.IsZero() {
				t.Fatalf("回落路径没读限流头")
			}
		})
	}
}

// TestNoHeaderMeansNoRetryAfter 上游没说就是零值，绝不编造。
//
// 编造一个到期时刻会让调度层把其实可用的目标锁住，
// 那比不知道到期时刻更坏。
func TestNoHeaderMeansNoRetryAfter(t *testing.T) {
	cases := []struct {
		name string
		h    http.Header
	}{
		{"nil 头", nil},
		{"空头", http.Header{}},
		{"头里是垃圾", func() http.Header {
			h := http.Header{}
			h.Set("Retry-After", "whenever")
			return h
		}()},
	}
	for _, name := range outboundNames() {
		for _, tc := range cases {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				c, _ := codec.Outbound(name)
				err := c.DecodeError(429, tc.h, rateLimitBody(t, name))
				if !err.RetryAfter.IsZero() {
					t.Fatalf("RetryAfter = %v，没给就必须是零值", err.RetryAfter)
				}
			})
		}
	}
}

// TestRetryAfterOnNonRateLimitStatus 限流头不限于 429。
//
// 503 带 Retry-After 是标准做法（RFC 9110），只在 429 上读会漏掉
// 过载场景——而过载恰恰是最需要按上游节奏退避的那一类。
func TestRetryAfterOnNonRateLimitStatus(t *testing.T) {
	for _, name := range outboundNames() {
		t.Run(name, func(t *testing.T) {
			c, _ := codec.Outbound(name)
			h := http.Header{}
			h.Set("Retry-After", "30")

			err := c.DecodeError(503, h, []byte(`{"error":{"message":"overloaded"}}`))
			if err.Kind != ir.ErrUpstream {
				t.Fatalf("kind = %q, want upstream", err.Kind)
			}
			if err.RetryAfter.IsZero() {
				t.Fatalf("503 上的 Retry-After 也必须被读到")
			}
		})
	}
}

// TestGeminiReadsRetryInfoFromBody Gemini 是唯一把到期时刻放在体内的。
func TestGeminiReadsRetryInfoFromBody(t *testing.T) {
	body := []byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED",` +
		`"message":"Quota exceeded","details":[` +
		`{"@type":"type.googleapis.com/google.rpc.ErrorInfo",` +
		`"reason":"RATE_LIMIT_EXCEEDED"},` +
		`{"@type":"type.googleapis.com/google.rpc.RetryInfo",` +
		`"retryDelay":"42s"}]}}`)

	c, _ := codec.Outbound(codec.ProtocolGemini)
	before := time.Now()
	err := c.DecodeError(429, nil, body)
	after := time.Now()

	if err.RetryAfter.IsZero() {
		t.Fatalf("RetryInfo.retryDelay 没被读到")
	}
	lo, hi := before.Add(42*time.Second), after.Add(42*time.Second)
	if err.RetryAfter.Before(lo) || err.RetryAfter.After(hi) {
		t.Fatalf("RetryAfter = %v，应落在 [%v, %v]", err.RetryAfter, lo, hi)
	}
}

// TestGeminiRetryInfoFractionalDelay 真实上游回的是小数秒串。
//
// 单列一格：整数秒与 "0.201506475s" 都要能解，而 Go 的 ParseDuration
// 两者都认——这一格钉住的是「我们真的用了 ParseDuration 而不是
// 自己按整数解析」。
func TestGeminiRetryInfoFractionalDelay(t *testing.T) {
	body := []byte(`{"error":{"code":429,"message":"Quota exceeded","details":[` +
		`{"@type":"type.googleapis.com/google.rpc.RetryInfo",` +
		`"retryDelay":"0.201506475s"}]}}`)

	c, _ := codec.Outbound(codec.ProtocolGemini)
	err := c.DecodeError(429, nil, body)
	if err.RetryAfter.IsZero() {
		t.Fatalf("小数秒的 retryDelay 没被读到")
	}
}

// TestGeminiIgnoresOtherDetailTypes details 里的其他成员不能被当成到期时刻。
//
// 数组里还有 ErrorInfo、QuotaFailure、BadRequest 等；宽松匹配 @type
// 会把别人的字段读成到期时刻，锁住一个其实可用的目标。
func TestGeminiIgnoresOtherDetailTypes(t *testing.T) {
	cases := map[string]string{
		"只有 ErrorInfo": `{"@type":"type.googleapis.com/google.rpc.ErrorInfo",` +
			`"reason":"RATE_LIMIT_EXCEEDED","retryDelay":"99s"}`,
		"QuotaFailure": `{"@type":"type.googleapis.com/google.rpc.QuotaFailure",` +
			`"retryDelay":"99s"}`,
		"类型名相近但不同": `{"@type":"type.googleapis.com/google.rpc.RetryInfoV2",` +
			`"retryDelay":"99s"}`,
		"RetryInfo 但 delay 为空": `{"@type":"type.googleapis.com/google.rpc.RetryInfo",` +
			`"retryDelay":""}`,
		"RetryInfo 但 delay 非法": `{"@type":"type.googleapis.com/google.rpc.RetryInfo",` +
			`"retryDelay":"soon"}`,
		"RetryInfo 但 delay 为零": `{"@type":"type.googleapis.com/google.rpc.RetryInfo",` +
			`"retryDelay":"0s"}`,
		"RetryInfo 但 delay 超上限": `{"@type":"type.googleapis.com/google.rpc.RetryInfo",` +
			`"retryDelay":"90000s"}`,
	}
	c, _ := codec.Outbound(codec.ProtocolGemini)
	for name, detail := range cases {
		t.Run(name, func(t *testing.T) {
			body := []byte(`{"error":{"code":429,"message":"Quota exceeded",` +
				`"details":[` + detail + `]}}`)
			err := c.DecodeError(429, nil, body)
			if !err.RetryAfter.IsZero() {
				t.Fatalf("RetryAfter = %v，不该从这一项读出时刻", err.RetryAfter)
			}
		})
	}
}

// TestHeaderAndBodyTakeTheEarlier 体内与头都给了时取更早的。
//
// 两处都在回答同一个问题「下一次最早什么时候」，不该因来源不同换规则。
func TestHeaderAndBodyTakeTheEarlier(t *testing.T) {
	body := []byte(`{"error":{"code":429,"message":"Quota exceeded","details":[` +
		`{"@type":"type.googleapis.com/google.rpc.RetryInfo",` +
		`"retryDelay":"3600s"}]}}`)
	c, _ := codec.Outbound(codec.ProtocolGemini)

	t.Run("头更早", func(t *testing.T) {
		h := http.Header{}
		h.Set("Retry-After", "60")
		err := c.DecodeError(429, h, body)
		if d := time.Until(err.RetryAfter); d > 5*time.Minute {
			t.Fatalf("RetryAfter 距今 %v，应采信更早的头（约 60s）", d)
		}
	})

	t.Run("体更早", func(t *testing.T) {
		h := http.Header{}
		h.Set("Retry-After", "3600")
		shortBody := []byte(`{"error":{"code":429,"message":"Quota exceeded","details":[` +
			`{"@type":"type.googleapis.com/google.rpc.RetryInfo",` +
			`"retryDelay":"30s"}]}}`)
		err := c.DecodeError(429, h, shortBody)
		if d := time.Until(err.RetryAfter); d > 5*time.Minute {
			t.Fatalf("RetryAfter 距今 %v，应采信更早的体内值（约 30s）", d)
		}
	})
}

// TestWithRetryAfterDoesNotOverwriteWithLaterValue 出口不能用更晚的头覆盖更早的体内值。
//
// 与上一条的「体更早」是同一判据的直接单测：那一条走完整 DecodeError，
// 这一条直接打在出口函数上，确保覆盖规则本身正确而不是恰好被路径掩护。
func TestWithRetryAfterDoesNotOverwriteWithLaterValue(t *testing.T) {
	early := time.Now().Add(time.Minute)
	err := &ir.Error{Kind: ir.ErrRateLimit, RetryAfter: early}

	h := http.Header{}
	h.Set("Retry-After", "3600")
	got := codec.WithRetryAfter(err, h)

	if !got.RetryAfter.Equal(early) {
		t.Fatalf("RetryAfter = %v, want %v（更早的那个）", got.RetryAfter, early)
	}
}
