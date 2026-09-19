// Package responses 实现 OpenAI Responses 协议的入站与出站编解码。
//
// 与 IR 的落差有两处。请求侧：本协议把消息、工具调用、工具结果、推理都拍平成
// input 数组里的独立条目，而 IR 把它们表达为消息内的块，所以两个方向都是
// 一对多的展开与合并。流式侧：增量用 (output_index, content_index) 两级定位，
// 入站要把它折成 IR 的一级块索引，出站要为每个条目补齐 added/done 成对帧
// 并在终止帧带上完整的 response 对象。
package responses

import (
	"net/http"
	"strings"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// Name 也用作 Thinking.SignatureFrom 的取值：加密的推理内容只在同族协议间透传。
const Name = codec.ProtocolResponses

type inboundCodec struct{}

func (inboundCodec) Name() string { return Name }

func (inboundCodec) DecodeRequest(body []byte) (*ir.Request, error) {
	return DecodeRequest(body)
}

// NewStreamEncoder 忽略请求：本协议的 usage 挂在 response.completed 上，
// 是协议固有形状而不是可选帧，没有对应的客户端开关可读。
func (inboundCodec) NewStreamEncoder(*ir.Request) codec.StreamEncoder { return newStreamEncoder() }

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
	// 本协议用 encrypted_content 承载签名，异族来源才丢。
	notes := codec.DescribeResponseSignatureLoss(resp, Name, true)
	// 但 function_call 条目上没有签名字段，所以工具调用那一位一律丢弃。
	notes = append(notes, codec.DescribeResponseToolSignatureLoss(resp, Name, false)...)
	return body, codec.DedupeNotes(notes), nil
}

func (inboundCodec) RenderError(err *ir.Error) (int, []byte) { return RenderError(err) }

func (inboundCodec) RenderStreamError(err *ir.Error) [][]byte { return RenderStreamError(err) }

type outboundCodec struct{}

func (outboundCodec) Name() string { return Name }

// Caps 里 ThinkingSig 为真但语义与 Anthropic 不同：本协议的签名是
// encrypted_content。StopSequences 为假——本协议没有停止序列字段。
func (outboundCodec) Caps() codec.Capabilities {
	return codec.Capabilities{
		Thinking:    true,
		ThinkingSig: true,
		Tools:       true,
		Images:      true,
		// instructions 是单一字符串，system 里的非文本块必须先降级成文本。
		SystemAsText: true,
		// 本协议独有三项：verbosity（text.verbosity）、include、truncation，
		// 另有客户端自定义 metadata。结构化输出在 text.format 下而非顶层
		// response_format。penalty / seed / n / logit_bias 本协议没有。
		ServiceTier:       true,
		ParallelToolCalls: true,
		ResponseFormat:    true,
		ResponseSchema:    true,
		Verbosity:         true,
		Include:           true,
		Truncation:        true,
		ClientMetadata:    true,
		// LogProbs 为真只覆盖 top_logprobs：本协议无独立的 logprobs 开关，
		// 给了 top_logprobs 即表示要对数概率。客户端只给了开关时出站补
		// 一个档位（见 LogProbsViaTopN）。
		LogProbs:        true,
		LogProbsViaTopN: true,
		// input_image 有 detail 层级，与 chat_completions 同名同义。
		ImageDetail: true,
		// ThinkingExcludesForcedTools 留零值：无账号、无官方文档，
		// 推理与强制工具是否互斥**未核实**。零值不等于已确认允许，
		// 拿到能发请求的账号后要补实测，别把它当成已有结论。
		// 图片走 input_image，wav/mp3 走 input_audio，其余走 input_file。
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
	return strings.TrimRight(baseURL, "/") + "/responses", nil
}

func (outboundCodec) NewStreamDecoder() codec.StreamDecoder { return newStreamDecoder() }

func (outboundCodec) DecodeResponse(body []byte) (*ir.Response, error) { return DecodeResponse(body) }

func (outboundCodec) DecodeError(status int, header http.Header, body []byte) *ir.Error {
	return DecodeError(status, header, body)
}

func init() {
	codec.RegisterInbound(inboundCodec{})
	codec.RegisterOutbound(outboundCodec{})
}
