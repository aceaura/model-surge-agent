// Package codec 定义协议编解码接口与注册表。
//
// 协议名与上游配置中心的 protocol 枚举一致，因此 dispatch 返回的
// target.protocol 可直接用来查出站 codec，无需映射表。
package codec

import (
	"strings"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

const (
	ProtocolAnthropic       = "anthropic"
	ProtocolChatCompletions = "chat_completions"
	ProtocolResponses       = "responses"
	ProtocolGemini          = "gemini"
)

// StreamEncoder 把 IR 事件编码成客户端协议的 SSE 帧。
// 每次 Encode 可能产出 0 到多帧（一个 IR 事件在某些协议里需要多帧表达）。
type StreamEncoder interface {
	Encode(ev ir.Event) ([][]byte, error)
	// Finish 补齐未闭合的块与终止帧。committed 之后出错时也要调用，
	// 否则客户端会一直等一个永不到来的结束帧。
	Finish() [][]byte
}

// StreamDecoder 把上游 SSE 帧解码成 IR 事件。
type StreamDecoder interface {
	// Feed 接收一帧。event 是 SSE 的 event 名（无名协议传空串），data 是 data 行内容。
	// 返回该帧产出的事件，可能为空（如保活帧）。
	Feed(event, data string) ([]ir.Event, error)
	// Finish 处理流正常结束时的收尾，补齐上游未显式发送的终止事件。
	Finish() []ir.Event
}

// InboundCodec 面向客户端：解请求、编响应。
type InboundCodec interface {
	Name() string
	DecodeRequest(body []byte) (*ir.Request, error)
	NewStreamEncoder() StreamEncoder
	EncodeResponse(resp *ir.Response) ([]byte, error)
	// RenderError 编码非流式错误响应，返回 HTTP 状态码与响应体。
	RenderError(err *ir.Error) (int, []byte)
	// RenderStreamError 编码流内错误帧。此时 HTTP 200 已写出，状态码不可变，
	// 错误只能以客户端协议的流内错误形式表达。
	RenderStreamError(err *ir.Error) [][]byte
}

// Capabilities 声明出站协议能表达什么，供编码时决定丢弃哪些 IR 字段。
type Capabilities struct {
	Thinking      bool
	ThinkingSig   bool
	Tools         bool
	Images        bool
	CacheControl  bool
	TopK          bool
	StopSequences bool
	// MediaTypes 是本协议接受的 media type 白名单。nil 表示只接受 image/*。
	// 白名单而非黑名单：上游对不认得的类型多回不可重试的 400，
	// 而不可重试意味着换目标也救不回来，只能在发出前降级。
	MediaTypes []string
}

// AcceptsMedia 判断本协议能否原生承载该 media type。
// 类型为空视为不能：多数协议的 mime 字段是必填的，谎报或留空都会被拒收。
func (c Capabilities) AcceptsMedia(mediaType string) bool {
	if !c.Images || mediaType == "" {
		return false
	}
	if c.MediaTypes == nil {
		return strings.HasPrefix(mediaType, "image/")
	}
	for _, t := range c.MediaTypes {
		if t == mediaType {
			return true
		}
	}
	return false
}

// LossyEncoder 是可选接口。出站 codec 实现它即可在编码请求时报告
// 因协议表达能力不足而丢弃的字段。不实现等价于「不丢任何字段」。
type LossyEncoder interface {
	// EncodeRequestLossy 除请求体外返回去重、已排序的有损说明。
	// 无丢弃时说明为 nil，且返回的请求体必须与 EncodeRequest 逐字节相同。
	EncodeRequestLossy(req *ir.Request) ([]byte, []string, error)
}

// OutboundCodec 面向上游：编请求、解响应。
type OutboundCodec interface {
	Name() string
	Caps() Capabilities
	EncodeRequest(req *ir.Request) ([]byte, error)
	// Endpoint 给出完整请求 URL 与该协议要求的额外头。
	// 凭据头来自上游配置中心，不在此处生成。
	Endpoint(baseURL, nativeModel string, stream bool) (string, map[string]string)
	NewStreamDecoder() StreamDecoder
	DecodeResponse(body []byte) (*ir.Response, error)
	// DecodeError 把上游错误响应归一成 ir.Error。
	DecodeError(status int, body []byte) *ir.Error
}
