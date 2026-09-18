package codec

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// KindForStatus 按 HTTP 状态码归类错误，并对 400 额外看消息内容：
// 上下文超限与普通参数错误都是 400，但前者不该记作目标的失败。
//
// 四个协议共用这套映射：状态码语义是 HTTP 层的，与协议无关，
// 各协议只在「错误体长什么样」上有差异。
func KindForStatus(status int, message string) ir.ErrorKind {
	switch {
	case status == http.StatusTooManyRequests:
		return ir.ErrRateLimit
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return ir.ErrAuth
	case status == http.StatusNotFound:
		return ir.ErrNotFound
	case status == http.StatusRequestEntityTooLarge:
		return ir.ErrContextExceeded
	case status == http.StatusBadRequest:
		if IsContextOverflow(message) {
			return ir.ErrContextExceeded
		}
		return ir.ErrInvalidRequest
	case status >= 500:
		return ir.ErrUpstream
	case status == 0:
		return ir.ErrUpstream
	default:
		return ir.ErrInvalidRequest
	}
}

// contextOverflowMarkers 是各家表达「输入太长」的说法。
// 没有统一错误码，只能匹配消息文本。
var contextOverflowMarkers = []string{
	"context length",
	"context_length",
	"context window",
	"maximum context",
	"too many tokens",
	"prompt is too long",
	"input length",
	"reduce the length",
	"exceeds the maximum",
}

func IsContextOverflow(message string) bool {
	m := strings.ToLower(message)
	for _, marker := range contextOverflowMarkers {
		if strings.Contains(m, marker) {
			return true
		}
	}
	return false
}

// codeKinds 把各家的错误码/状态串映射到 IR 分类。
//
// 流内错误帧没有 HTTP 状态码，只有这个字符串可依据。不认它就全归 upstream，
// 于是限流会被当成目标故障去累计失败并冷却，而真正该做的是等一等再发。
var codeKinds = map[string]ir.ErrorKind{
	"rate_limit_error":        ir.ErrRateLimit,
	"rate_limit_exceeded":     ir.ErrRateLimit,
	"insufficient_quota":      ir.ErrRateLimit,
	"resource_exhausted":      ir.ErrRateLimit,
	"authentication_error":    ir.ErrAuth,
	"permission_error":        ir.ErrAuth,
	"permission_denied":       ir.ErrAuth,
	"invalid_api_key":         ir.ErrAuth,
	"unauthenticated":         ir.ErrAuth,
	"not_found_error":         ir.ErrNotFound,
	"model_not_found":         ir.ErrNotFound,
	"not_found":               ir.ErrNotFound,
	"invalid_request_error":   ir.ErrInvalidRequest,
	"invalid_argument":        ir.ErrInvalidRequest,
	"context_length_exceeded": ir.ErrContextExceeded,
	"overloaded_error":        ir.ErrUpstream,
	"api_error":               ir.ErrUpstream,
	"unavailable":             ir.ErrUpstream,
	"internal":                ir.ErrUpstream,
	"deadline_exceeded":       ir.ErrTimeout,
	"timeout":                 ir.ErrTimeout,
}

// KindForCode 按错误码/状态串归类，未知码返回 false 交回调用方。
func KindForCode(code string) (ir.ErrorKind, bool) {
	kind, ok := codeKinds[strings.ToLower(strings.TrimSpace(code))]
	return kind, ok
}

// KindFor 归类一个上游错误：有 HTTP 状态码就以它为准，没有（流内错误帧）
// 才退到体内的错误码/状态串。
//
// 缺了后半段，流内的限流会被 KindForStatus(0) 归成 upstream：调度层据此
// 累计目标失败并冷却，而限流本该只是等一等再发。
func KindFor(status int, code, message string) ir.ErrorKind {
	if status != 0 {
		return KindForStatus(status, message)
	}
	if kind, ok := KindForCode(code); ok {
		// 参数错误里混着上下文超限，后者不该记作目标的失败。
		if kind == ir.ErrInvalidRequest && IsContextOverflow(message) {
			return ir.ErrContextExceeded
		}
		return kind
	}
	if IsContextOverflow(message) {
		return ir.ErrContextExceeded
	}
	return ir.ErrUpstream
}

// StatusMessage 在错误体无法解析时（网关返回 HTML 之类）给出可读消息，
// 截断以防把整个页面写进日志。
func StatusMessage(status int, body []byte) string {
	const maxLen = 512
	text := strings.TrimSpace(string(body))
	if text == "" {
		return fmt.Sprintf("upstream returned %d", status)
	}
	if len(text) > maxLen {
		text = text[:maxLen]
	}
	return fmt.Sprintf("upstream returned %d: %s", status, text)
}

// StatusForKind 是 KindForStatus 的反向映射，供入站 codec 决定响应状态码。
func StatusForKind(kind ir.ErrorKind) int {
	switch kind {
	case ir.ErrInvalidRequest, ir.ErrContextExceeded:
		return http.StatusBadRequest
	case ir.ErrAuth:
		return http.StatusUnauthorized
	case ir.ErrNotFound:
		return http.StatusNotFound
	case ir.ErrRateLimit:
		return http.StatusTooManyRequests
	case ir.ErrTimeout:
		return http.StatusGatewayTimeout
	default:
		return http.StatusInternalServerError
	}
}
