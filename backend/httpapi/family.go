package httpapi

import (
	"net/http"

	"github.com/aceaura/model-surge-agent/backend/codec"
)

// familyAuto 表示这条路径不指定协议族，要按请求信号推断。
const familyAuto = ""

// clientFamily 判断这次请求该用哪个协议族的外形。
//
// 需要推断是因为 /v1/models 与 /models 两族 SDK 都会打：用户把 base_url
// 配成 host，Anthropic SDK 与 OpenAI SDK 各自拼出同一个路径，而两家清单的
// 字段名与包装都不同，给错了对方解析不出来。
//
// 路径优先于头：/anthropic/... 与 /openai/... 是用户显式选的族，头只是推断。
//
// 头信号的次序固定 anthropic → gemini → openai：前两者要有专有头才成立，
// openai 是「什么专有信号都没有」的那一档，只能垫底。三家的凭据头名互不
// 重叠（Anthropic 的 x-api-key、Gemini 的 x-goog-api-key 或 query key、
// OpenAI 的 Authorization: Bearer），所以单看头名就能分族。
func clientFamily(r *http.Request, pathFamily string) string {
	if pathFamily != familyAuto {
		return pathFamily
	}
	switch {
	case r.Header.Get("anthropic-version") != "", r.Header.Get("x-api-key") != "":
		return codec.ProtocolAnthropic
	case r.Header.Get("x-goog-api-key") != "", r.URL.Query().Get("key") != "":
		return codec.ProtocolGemini
	default:
		return codec.ProtocolChatCompletions
	}
}

// envelopeProtocol 把协议族映射到能渲染错误信封的入站协议。
//
// gemini 只注册了出站 codec，清单路径上它只是个外形标记；这类请求出错时
// 回 anthropic 信封，判据与 protocolForPath 的兜底相同——认错协议的代价
// （SDK 报字段不认识）小于回一段解不动的东西。
func envelopeProtocol(family string) string {
	if family == codec.ProtocolGemini {
		return codec.ProtocolAnthropic
	}
	return family
}
