// Package chatcompletions 实现 OpenAI Chat Completions 协议的入站与出站编解码。
//
// 与 IR 的主要落差是流式模型：本协议没有块生命周期，正文、推理与工具入参
// 都以 choices[].delta 的隐式续写形式出现。入站方向靠槽位分配补出块边界，
// 出站方向把块索引压掉，只保留工具调用的序号——那是客户端拼回分片的唯一依据。
package chatcompletions

import (
	"strings"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// Name 也用作 Thinking.SignatureFrom 的取值：签名只在同族协议间透传。
const Name = codec.ProtocolChatCompletions

type inboundCodec struct{}

func (inboundCodec) Name() string { return Name }

func (inboundCodec) DecodeRequest(body []byte) (*ir.Request, error) {
	return DecodeRequest(body)
}

func (inboundCodec) NewStreamEncoder() codec.StreamEncoder { return newStreamEncoder() }

// EncodeResponse 是 EncodeResponseLossy 的包装：两条路径共用同一编码，
// 响应体逐字节相同，否则客户端看到的内容会因诊断开关而漂移。
func (c inboundCodec) EncodeResponse(resp *ir.Response) ([]byte, error) {
	body, _, err := c.EncodeResponseLossy(resp)
	return body, err
}

func (inboundCodec) EncodeResponseLossy(resp *ir.Response) ([]byte, []string, error) {
	body, err := EncodeResponse(resp)
	if err != nil {
		return nil, nil, err
	}
	// 本协议无签名字段，一律丢弃，与来源无关。
	return body, codec.DescribeResponseSignatureLoss(resp, Name, false), nil
}

func (inboundCodec) RenderError(err *ir.Error) (int, []byte) { return RenderError(err) }

func (inboundCodec) RenderStreamError(err *ir.Error) [][]byte { return RenderStreamError(err) }

type outboundCodec struct{}

func (outboundCodec) Name() string { return Name }

// Caps 关掉 ThinkingSig 与 CacheControl：本协议没有承载它们的字段。
// TopK 同样没有——各家的兼容实现虽偶有支持，但不是协议的一部分，
// 需要时应通过上游模型配置的 overrides 显式加上。
func (outboundCodec) Caps() codec.Capabilities {
	return codec.Capabilities{
		Thinking:      true,
		Tools:         true,
		Images:        true,
		StopSequences: true,
		// 官方 stop 数组至多 4 项，超出即 400。
		MaxStopSequences: 4,
		// 显式写出 false：实测本协议允许推理与强制工具共存（deepseek 上
		// tool_choice 具名 + 思考开启回 200，同时给出文本与 tool_use）。
		// 留空会让后来者以为只是没填，照 anthropic 抄成 true 就白丢推理。
		ThinkingExcludesForcedTools: false,
		// 图片走 image_url，wav/mp3 走 input_audio，其余走 file。
		// input_audio 只认这两种格式名，别的音频只能降级。
		MediaTypes: []string{
			"image/png", "image/jpeg", "image/gif", "image/webp",
			"audio/wav", "audio/mpeg",
			"application/pdf", "text/plain", "text/csv", "application/json",
		},
	}
}

// EncodeRequest 是 EncodeRequestLossy 丢弃诊断的包装：两条路径共用同一编码，
// 请求体逐字节相同，否则提示缓存前缀会因诊断开关而漂移。
func (c outboundCodec) EncodeRequest(req *ir.Request) ([]byte, error) {
	body, _, err := c.EncodeRequestLossy(req)
	return body, err
}

func (outboundCodec) EncodeRequestLossy(req *ir.Request) ([]byte, []string, error) {
	caps := outboundCodec{}.Caps()
	// 在副本上做结构调整：调用方的请求要留着换目标重试，不能被本次编码改写。
	shaped := req.Clone()
	shapeNotes := codec.ShapeRequest(shaped, Name, caps)
	body, err := EncodeRequest(shaped)
	if err != nil {
		return nil, nil, err
	}
	// 诊断按原始请求推导：shape 已把部分字段降级掉，拿改写后的请求去推
	// 会漏报本该报的丢弃。
	return body, codec.MergeNotes(codec.DescribeLossy(req, Name, caps), shapeNotes), nil
}

// Endpoint 的 stream 参数不影响路径：流式由请求体的 stream 字段决定。
func (outboundCodec) Endpoint(baseURL, _ string, _ bool) (string, map[string]string) {
	return strings.TrimRight(baseURL, "/") + "/chat/completions", nil
}

func (outboundCodec) NewStreamDecoder() codec.StreamDecoder { return newStreamDecoder() }

func (outboundCodec) DecodeResponse(body []byte) (*ir.Response, error) { return DecodeResponse(body) }

func (outboundCodec) DecodeError(status int, body []byte) *ir.Error {
	return DecodeError(status, body)
}

func init() {
	codec.RegisterInbound(inboundCodec{})
	codec.RegisterOutbound(outboundCodec{})
}
