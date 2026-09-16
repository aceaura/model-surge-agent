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
