package httpapi

import (
	"net/http"

	"github.com/aceaura/model-surge-agent/backend/codec"
)

// 每个入站协议给三条路径：裸路径、带 /v1、带厂商前缀。
//
// 三份别名不是冗余：客户端把 base_url 配成 host、host/v1、host/anthropic 的都有，
// 而多数客户端不允许改它拼在后面的固定路径。少一个别名就有一类客户端配不通。
// 还多给一条 /v1/v1：用户把 base_url 配成 host/v1 时，多数 SDK 仍会
// 在后面拼一个固定的 /v1/...，于是路径里出现两段。裸路径别名接不住它，
// 因为 SDK 拼的那段是它自己加的、用户改不掉。
var messagesPaths = []string{
	"/v1/messages",
	"/v1/v1/messages",
	"/anthropic/v1/messages",
	"/messages",
}

var countTokensPaths = []string{
	"/v1/messages/count_tokens",
	"/v1/v1/messages/count_tokens",
	"/anthropic/v1/messages/count_tokens",
	"/messages/count_tokens",
}

var chatPaths = []string{
	"/v1/chat/completions",
	"/v1/v1/chat/completions",
	"/openai/v1/chat/completions",
	"/chat/completions",
}

var responsesPaths = []string{
	"/v1/responses",
	"/v1/v1/responses",
	"/openai/v1/responses",
	"/responses",
}

// modelPaths 把清单路径映射到要渲染成哪个协议的外形。
//
// 清单的字段名各家不同（Anthropic 的 display_name、OpenAI 的 owned_by、
// Gemini 的 displayName），SDK 解析不出自己认识的形状就会报错。
//
// 带族前缀的路径钉死外形，无前缀的取 familyAuto 按请求头推断：/v1/models
// 与 /models 是两族 SDK 共用的路径，钉死任一族都会让另一族解析失败。
// 判定见 clientFamily。
var modelPaths = map[string]string{
	"/v1/models":           familyAuto,
	"/v1/v1/models":        familyAuto,
	"/models":              familyAuto,
	"/anthropic/v1/models": codec.ProtocolAnthropic,
	"/openai/v1/models":    codec.ProtocolChatCompletions,
	// 只有 Gemini 客户端会打 /v1beta，无需推断。
	"/v1beta/models": codec.ProtocolGemini,
}

// modelIDSuffix 是单模型查询在清单路径之后的那一段。
//
// 用 {id...} 通配而非 {id}：Gemini 客户端会把清单里的资源名 models/xxx
// 原样回传，那里面带斜杠，单段模式匹配不到。
const modelIDSuffix = "/{id...}"

// register 把一组别名指向同一个处理函数，并限定方法。
func register(mux *http.ServeMux, method string, paths []string, h http.HandlerFunc) {
	for _, p := range paths {
		mux.HandleFunc(method+" "+p, h)
	}
}
