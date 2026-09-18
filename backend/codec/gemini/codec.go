// Package gemini 实现 Gemini generateContent 协议的出站编解码。
//
// 只有出站：本服务不对客户端暴露 Gemini 接口，它只作为上游协议出现。
//
// 与另外三个协议的差异集中在三处。寻址：模型名在 URL 路径里，流式由方法名
// :streamGenerateContent 加 alt=sse 决定，请求体里既没有 model 也没有 stream。
// 内容结构：part 是判别式联合体，判别位是「哪个字段非空」，推理用 text part 上的
// thought 标记表达。工具回指：functionResponse 只有函数名没有调用 id，
// 所以编码请求要先扫出 id→name 表，解码响应要为调用合成 id。
package gemini

import (
	"strings"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/codec/schemadialect"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// Name 也用作 Thinking.SignatureFrom 的取值：thoughtSignature 只在本族协议间透传。
const Name = codec.ProtocolGemini

type outboundCodec struct{}

func (outboundCodec) Name() string { return Name }

// Caps 里 CacheControl 为假：本协议的缓存是显式的 cachedContent 资源，
// 得先创建再引用，不是请求体里的逐块标记，无法从 IR 的 cache_control 直译。
func (outboundCodec) Caps() codec.Capabilities {
	return codec.Capabilities{
		Thinking:      true,
		ThinkingSig:   true,
		Tools:         true,
		Images:        true,
		TopK:          true,
		StopSequences: true,
		// systemInstruction 是单一 Content，system 里的非文本块必须先降级成文本。
		SystemAsText: true,
		// functionCall / functionResponse 靠 name 配对，id 是可选字段：
		// 本服务合成的 id 不写进请求体，交由上游按调用顺序消歧。
		ToolIDOptional: true,
		// ThinkingExcludesForcedTools 留零值：无账号、无官方文档，
		// 推理与强制工具是否互斥**未核实**。零值不等于已确认允许，
		// 拿到能发请求的账号后要补实测，别把它当成已有结论。
		// 本协议的 schema 是 OpenAPI 3.0 子集，不是完整 JSON Schema：
		// 表外关键字会被当成未知字段拒收（400 Invalid JSON payload），
		// type 取值必须大写，也不接受联合 type 数组。
		SchemaDialect: schemadialect.Dialect{
			Drop: []string{
				"$schema", "$id", "additionalProperties", "patternProperties",
				"minLength", "maxLength", "minItems", "maxItems",
				"exclusiveMinimum", "exclusiveMaximum", "deprecated", "title",
			},
			UppercaseType:       true,
			CollapseUnionType:   true,
			OmitEmptyProperties: true,
		},
		// 本协议的 mimeType 是必填项且上游按白名单校验，
		// 不在表里的类型发出去会拿到不可重试的 400。
		MediaTypes: []string{
			"image/png", "image/jpeg", "image/webp", "image/heic", "image/heif",
			"audio/wav", "audio/mpeg", "audio/mp3", "audio/aiff", "audio/aac",
			"audio/ogg", "audio/flac",
			"video/mp4", "video/mpeg", "video/mov", "video/avi", "video/webm",
			"application/pdf",
			"text/plain", "text/csv", "text/html", "text/markdown",
			"application/json", "text/xml",
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

// Endpoint 与另外三个协议不同：模型名进路径，流式换方法名并加 alt=sse。
// 所以这里的两个参数都不能忽略。
func (outboundCodec) Endpoint(baseURL, nativeModel string, stream bool) (string, map[string]string) {
	base := strings.TrimRight(baseURL, "/") + "/models/" + nativeModel
	if stream {
		return base + ":streamGenerateContent?alt=sse", nil
	}
	return base + ":generateContent", nil
}

func (outboundCodec) NewStreamDecoder() codec.StreamDecoder { return newStreamDecoder() }

func (outboundCodec) DecodeResponse(body []byte) (*ir.Response, error) { return DecodeResponse(body) }

func (outboundCodec) DecodeError(status int, body []byte) *ir.Error {
	return DecodeError(status, body)
}

func init() {
	codec.RegisterOutbound(outboundCodec{})
}
