package codec

import (
	"encoding/json"
	"strings"

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
