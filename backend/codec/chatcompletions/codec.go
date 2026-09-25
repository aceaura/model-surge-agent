// Package chatcompletions 实现 OpenAI Chat Completions 协议的入站与出站编解码。
//
// 与 IR 的主要落差是流式模型：本协议没有块生命周期，正文、推理与工具入参
// 都以 choices[].delta 的隐式续写形式出现。入站方向靠槽位分配补出块边界，
// 出站方向把块索引压掉，只保留工具调用的序号——那是客户端拼回分片的唯一依据。
package chatcompletions

import (
	"net/http"
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

// NewStreamEncoder 读客户端对单独 usage 帧的表态。
// 只有明确的 false 才不发：没给与明确 true 都发（本服务的既有行为）。
func (inboundCodec) NewStreamEncoder(req *ir.Request) codec.StreamEncoder {
	e := newStreamEncoder()
	if req != nil && req.IncludeUsage != nil && !*req.IncludeUsage {
		e.suppressUsageFrame = true
	}
	return e
}

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
	// 本协议无签名字段，一律丢弃，与来源无关。思考块与工具调用两处都是。
	notes := codec.DescribeResponseSignatureLoss(resp, Name, false)
	notes = append(notes, codec.DescribeResponseToolSignatureLoss(resp, Name, false)...)
	// 助手消息没有附件形态，模型产出的附件整块消失。
	if images, files := codec.CountResponseMedia(resp); images > 0 || files > 0 {
		notes = append(notes, codec.MediaOutputDropNote(images, files))
	}
	// 托管工具块没有本族输出形态，编码器整块跳过：丢了要报出来。
	if calls, results := codec.CountResponseServerTools(resp); calls > 0 || results > 0 {
		notes = append(notes, codec.ServerToolDropNote(calls, results))
	}
	// 畸形工具参数：arguments 是字符串槽位，原文透传，报出不可安全执行。
	notes = append(notes, codec.DescribeResponseToolArgsLoss(resp, false)...)
	return body, codec.DedupeNotes(notes), nil
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
		Thinking: true,
		Tools:    true,
		// role:tool 消息不接受媒体 part：编出 image_url 会被上游按格式错误
		// 拒收整个请求（cc-switch 也明确写了这一条）。
		ToolResultTextOnly: true,
		Images:             true,
		StopSequences:      true,
		// 官方 stop 数组至多 4 项，超出即 400。
		MaxStopSequences: 4,
		// 本协议是这批调参字段的来源协议，除三个 responses 专有项
		// （verbosity / include / truncation）与 metadata 外全部承载。
		Penalties:  true,
		Seed:       true,
		Candidates: true,
		LogProbs:   true,
		// image_url.detail 决定识别精度与计费档位。
		ImageDetail:       true,
		LogitBias:         true,
		ServiceTier:       true,
		ParallelToolCalls: true,
		ResponseFormat:    true,
		ResponseSchema:    true,
		// message.annotations 是本协议的来源标注槽位（url_citation）。
		// 允许无范围标注，比 anthropic 宽松，比 responses 多 cited_text。
		Citations: true,
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
	// 体积在编码之后才测得到：IR 的估算值与实际序列化结果有偏差
	// （JSON 转义、base64 媒体、字段名开销），而偏差正是这条预检要防的。
	if note := codec.PayloadBudgetNote(body, Name, caps); note != "" {
		shapeNotes = append(shapeNotes, note)
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

// DecodeResponseLossy 实现 codec.LossyResponseDecoder：本协议的响应里有
// choices 数组，n>1 时多出来的候选在解码时被丢掉。
func (outboundCodec) DecodeResponseLossy(body []byte) (*ir.Response, []string, error) {
	return DecodeResponseLossy(body)
}

func (outboundCodec) DecodeError(status int, header http.Header, body []byte) *ir.Error {
	return DecodeError(status, header, body)
}

func init() {
	codec.RegisterInbound(inboundCodec{})
	codec.RegisterOutbound(outboundCodec{})
}
