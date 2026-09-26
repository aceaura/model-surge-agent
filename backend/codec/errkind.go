package codec

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/aceaura/model-surge-agent/backend/ir"
	"github.com/aceaura/model-surge-agent/backend/textsafe"
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
	// P1：402 是「当前账号欠费 / 额度耗尽」，换号是唯一正确动作。归 rate_limit 族
	// 让 outcome 从 abnormal（不换号、白烧当前账号直到人工介入）变 retrying（换号重试）。
	case status == http.StatusPaymentRequired:
		return ir.ErrRateLimit
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return ir.ErrAuth
	case status == http.StatusNotFound:
		return ir.ErrNotFound
	case status == http.StatusRequestEntityTooLarge:
		return ir.ErrContextExceeded
	// P2：408（请求超时）换目标合理，425（Too Early，TLS 早期数据被拒）极罕见但重试
	// 无害。归 timeout 族让 outcome 从 abnormal 变 retrying；重试次数受 MaxAttempts 兜底。
	case status == http.StatusRequestTimeout, status == http.StatusTooEarly:
		return ir.ErrTimeout
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
	// Anthropic 的另一种文案，与 "prompt is too long" 并存。
	"request is too long",
	// Gemini 形态。
	"input token count exceeds",
	"exceeds the context",
	"context limit",
}

// 「token limit」单独出现不算上下文超限：同一措辞也用于速率限制
// （「每分钟 token limit」），误判会把该等待重试的 429 变成不换目标的 400。
// 只有它伴随上下文语境与一个超出类动词才算。做法采自 sub2api 的组合式判定
// （openai_gateway_upstream_errors.go:196-218），它同样不信裸关键词。
var overflowVerbs = []string{"exceed", "too long", "too large"}

func hasOverflowVerb(m string) bool {
	for _, v := range overflowVerbs {
		if strings.Contains(m, v) {
			return true
		}
	}
	return false
}

func IsContextOverflow(message string) bool {
	m := strings.ToLower(message)
	for _, marker := range contextOverflowMarkers {
		if strings.Contains(m, marker) {
			return true
		}
	}
	if strings.Contains(m, "token limit") &&
		strings.Contains(m, "context") && hasOverflowVerb(m) {
		return true
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
	// P3：风控 / 内容安全拦截码。归 ErrContentFilter（不可重试、outcome=abnormal），
	// 而不是落到 default=ErrUpstream（可重试）：否则调度器会换号重发一个永远被策略
	// 挡下的请求，把整个账号池白烧一遍。码表对齐 sub2api 与旧仓 4b6afce 的 cyber_policy
	// 特例。注意 content_filter 作「错误码」与作「finish_reason / incomplete reason」
	// 是两条不同的路径：后者是正常终止（StopContentFilter），只有错误体里的 code 才走这里。
	"cyber_policy":             ir.ErrContentFilter,
	"content_policy":           ir.ErrContentFilter,
	"content_policy_violation": ir.ErrContentFilter,
	"content_filter":           ir.ErrContentFilter,
	"overloaded_error":         ir.ErrUpstream,
	"api_error":                ir.ErrUpstream,
	"unavailable":              ir.ErrUpstream,
	"internal":                 ir.ErrUpstream,
	"deadline_exceeded":        ir.ErrTimeout,
	"timeout":                  ir.ErrTimeout,
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
	return fmt.Sprintf("upstream returned %d: %s", status,
		textsafe.Truncate(text, maxLen))
}

// StatusForKind 是 KindForStatus 的反向映射，供入站 codec 决定响应状态码。
func StatusForKind(kind ir.ErrorKind) int {
	switch kind {
	case ir.ErrInvalidRequest, ir.ErrContextExceeded, ir.ErrContentFilter:
		return http.StatusBadRequest
	case ir.ErrAuth:
		return http.StatusUnauthorized
	case ir.ErrNotFound:
		return http.StatusNotFound
	case ir.ErrRateLimit:
		return http.StatusTooManyRequests
	case ir.ErrTimeout:
		return http.StatusGatewayTimeout
	case ir.ErrTransport:
		// 502 而不是 ErrUpstream 的 500：500 是「上游出错了」，
		// 502 是「没连上上游」，客户端据此能分清要不要重发。
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}
