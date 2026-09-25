package codec

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/aceaura/model-surge-agent/backend/codec/ratelimit"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// messageFields 是各家放错误消息的字段名，按可信度排序。
//
// 只有 message 是三大协议共同的规范字段，其余都是网关与兼容层的自创写法。
// 转发链上每多一跳就可能换一种，靠猜不如把见过的都列出来：提不出消息就只能
// 把整段 body 塞给运维，那等于没归一化。
var messageFields = []string{
	"message",
	"msg",
	"error_msg",
	"errorMessage",
	"detail",
	"description",
	"err",
	"reason",
}

// ExtractMessage 从任意形状的上游错误体里挖出人能读的消息。
//
// 逐级下钻 error / response.error 这类包装层，再按 messageFields 的顺序取值。
// 提不到返回空串，由调用方回落到状态码描述——宁可给一句「upstream returned
// 503」，也不能产出一个消息为空的错误对象：那在流水里查不出任何东西。
func ExtractMessage(body []byte) string {
	var v any
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	return messageFrom(v, 0)
}

// RefineMessage 修一条已经从规范字段取到的消息。
//
// 上游常把下游的整个错误体字符串化塞进 message，此时里面才是真消息。
// 不是 JSON 就原样返回。
func RefineMessage(message string) string {
	if inner := unwrapJSONString(message, 0); inner != "" {
		return inner
	}
	return strings.TrimSpace(message)
}

// FallbackError 在协议自家的错误结构解析不出消息时兜底。
//
// 先尽力从任意形状里挖消息，挖不到才回落到状态码描述——绝不产出消息为空的
// 错误对象，那在流水里查不出任何东西。
func FallbackError(status int, body []byte) *ir.Error {
	message := ExtractMessage(body)
	if message == "" {
		message = StatusMessage(status, body)
	}
	return ir.NewError(KindForStatus(status, message), status, "", message)
}

// SalvagedError 是消息解不出、但同一 error 对象的其余字段解出来了的出口。
//
// 典型形态是代理把 message 写成数字：外层 body 已是合法 JSON，内层只可能
// 类型不匹配，而 Go 的解码器记下类型错误后仍会把其余键解完——code 其实
// 已经拿到了。拿 err != nil 当「什么都没解到」会把上游自报的错误码一并
// 丢掉：归因靠的是 code，回落原文只负责让失败可见，替代不了归因。
//
// 消息按 ExtractMessage → 状态码描述回落，与 FallbackError 同口径；
// 分类走 KindFor，与各协议 convertError 同判据：status 非零时与
// KindForStatus 等价，零时（流内帧）才按救回来的 code 归类。
func SalvagedError(status int, body []byte, code string) *ir.Error {
	message := ExtractMessage(body)
	if message == "" {
		message = StatusMessage(status, body)
	}
	return ir.NewError(KindFor(status, code, message), status, code, message)
}

// WithParam 给已归一的错误补上出问题的字段名，已有值时不覆盖。
//
// 放在 DecodeError 的出口统一调用，而不是各协议自己从 wire 结构里取：
// 只有 openai 系两个协议的 wireError 有 param 位，另两个没有，而错误体常来自
// 兼容层代理——anthropic 端点回一个 openai 形状的错误体是常态。从原始字节挖
// 能同时覆盖结构化路径与 FallbackError 路径。
func WithParam(err *ir.Error, body []byte) *ir.Error {
	if err == nil || err.Param != "" {
		return err
	}
	err.Param = ExtractParam(body)
	return err
}

// WithRetryAfter 给已归一的错误补上响应头里的最早可重试时刻。
//
// 与 WithParam 一样放在 DecodeError 的出口统一调用：限流头是 HTTP 层的，
// 每个协议都可能收到，让各协议自己解会解出四套判据。
//
// 错误上已有值（来自体内的结构化到期信息，如 Gemini 的 RetryInfo）时**取更早的
// 那个**，与 ratelimit.ResetAt 跨多个头取最早同口径：两处都在回答同一个问题
// 「下一次最早什么时候」，不该因为来源不同换一套规则。
//
// 头里没有可信值时保持原状——绝不编造，编造会把可用目标锁住。
func WithRetryAfter(err *ir.Error, h http.Header) *ir.Error {
	if err == nil || h == nil {
		return err
	}
	fromHeader := ratelimit.ResetAt(h, time.Now())
	if fromHeader.IsZero() {
		return err
	}
	if err.RetryAfter.IsZero() || fromHeader.Before(err.RetryAfter) {
		err.RetryAfter = fromHeader
	}
	return err
}

// ExtractParam 从上游错误体里取出出问题的请求字段名。
//
// 只认 error.param 与顶层 param 两种位置，与 ExtractMessage 的宽松策略刻意相反：
// message 认错了只是文案不准，param 认错了会把客户端引向一个根本没问题的字段，
// 让它照着改——那比不给这个字段更糟。
func ExtractParam(body []byte) string {
	var v map[string]any
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	if inner, ok := v["error"].(map[string]any); ok {
		if s, ok := inner["param"].(string); ok {
			return strings.TrimSpace(s)
		}
	}
	if s, ok := v["param"].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// maxUnwrapDepth 限制下钻层数：错误体来自外部，深层嵌套可能是恶意构造，
// 也可能只是层层转发的产物，两种情况都没有再往下找的价值。
const maxUnwrapDepth = 6

func messageFrom(v any, depth int) string {
	if depth > maxUnwrapDepth {
		return ""
	}
	switch t := v.(type) {
	case string:
		// 消息位上放着一段完整 JSON：上游把下游的错误体整个字符串化塞了进来。
		// 直接回显会让运维读到一堆转义引号，真正的消息还在里面。
		if inner := unwrapJSONString(t, depth); inner != "" {
			return inner
		}
		return strings.TrimSpace(t)
	case map[string]any:
		for _, key := range messageFields {
			if got := messageFrom(t[key], depth+1); got != "" {
				return got
			}
		}
		// 包装层本身不带消息，往下一层找。
		for _, key := range []string{"error", "response", "data", "body", "details", "errors"} {
			if got := messageFrom(t[key], depth+1); got != "" {
				return got
			}
		}
	case []any:
		// gemini 的 details 是数组，火山的部分网关也会返回错误列表。
		for _, item := range t {
			if got := messageFrom(item, depth+1); got != "" {
				return got
			}
		}
	}
	return ""
}

// unwrapJSONString 在字符串本身是 JSON 对象/数组时二次解包。
func unwrapJSONString(s string, depth int) string {
	trimmed := strings.TrimSpace(s)
	if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
		return ""
	}
	var inner any
	if json.Unmarshal([]byte(trimmed), &inner) != nil {
		return ""
	}
	return messageFrom(inner, depth+1)
}
