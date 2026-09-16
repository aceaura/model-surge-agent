package httpapi

import (
	"net/http"

	"github.com/aceaura/model-surge-agent/backend/codec"
)

// 每个入站协议给三条路径：裸路径、带 /v1、带厂商前缀。
//
// 三份别名不是冗余：客户端把 base_url 配成 host、host/v1、host/anthropic 的都有，
// 而多数客户端不允许改它拼在后面的固定路径。少一个别名就有一类客户端配不通。
var messagesPaths = []string{"/v1/messages", "/anthropic/v1/messages", "/messages"}

var countTokensPaths = []string{
	"/v1/messages/count_tokens",
	"/anthropic/v1/messages/count_tokens",
	"/messages/count_tokens",
}

var chatPaths = []string{"/v1/chat/completions", "/openai/v1/chat/completions", "/chat/completions"}

var responsesPaths = []string{"/v1/responses", "/openai/v1/responses", "/responses"}

// modelPaths 把清单路径映射到要渲染成哪个协议的外形。
//
// 外形按请求路径而非按 Accept 决定：清单的字段名各家不同（Anthropic 的
// display_name 与 OpenAI 的 owned_by），SDK 解析不出自己认识的形状就会报错。
var modelPaths = map[string]string{
	"/v1/models":           codec.ProtocolAnthropic,
	"/anthropic/v1/models": codec.ProtocolAnthropic,
	"/openai/v1/models":    codec.ProtocolChatCompletions,
	"/models":              codec.ProtocolChatCompletions,
}

// register 把一组别名指向同一个处理函数，并限定方法。
func register(mux *http.ServeMux, method string, paths []string, h http.HandlerFunc) {
	for _, p := range paths {
		mux.HandleFunc(method+" "+p, h)
	}
}
