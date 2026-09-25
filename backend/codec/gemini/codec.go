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
	"fmt"
	"net/http"
	"net/url"
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
		Thinking:    true,
		ThinkingSig: true,
		ToolCallSig: true,
		Tools:       true,
		// functionCall.args 是 JSON 对象槽位。
		ToolInputObject: true,
		Images:          true,
		TopK:            true,
		StopSequences:   true,
		// 官方限定至多 5 个 stopSequences，超出即 INVALID_ARGUMENT。
		// 不截断会让同一个请求「只有 Gemini 坏了」，换上游即成功。
		MaxStopSequences: 5,
		// functionResponse 的载荷用 error 键承载失败态（见 wrapResponse）。
		ToolResultError: true,
		// functionResponse.response 只有一个 output/error 字符串键，
		// 媒体块同样会被碾掉。
		ToolResultTextOnly: true,
		// systemInstruction 是单一 Content，system 里的非文本块必须先降级成文本。
		SystemAsText: true,
		// 本协议在 generationConfig 下有 candidateCount、responseLogprobs
		// 与 logprobs、responseMimeType 与 responseSchema、seed 与两个
		// penalty（键名是驼峰，语义与 OpenAI 同）。logit_bias、service_tier、
		// parallel_tool_calls 与三个 responses 专有项没有对应字段。
		Candidates:     true,
		LogProbs:       true,
		ResponseFormat: true,
		ResponseSchema: true,
		Seed:           true,
		Penalties:      true,
		// functionCall / functionResponse 靠 name 配对，id 是可选字段：
		// 本服务合成的 id 不写进请求体，交由上游按调用顺序消歧。
		ToolIDOptional: true,
		// ThinkingExcludesForcedTools 留零值：无账号、无官方文档，
		// 推理与强制工具是否互斥**未核实**。零值不等于已确认允许，
		// 拿到能发请求的账号后要补实测，别把它当成已有结论。
		// 本协议的 schema 是 OpenAPI 3.0 子集，不是完整 JSON Schema：
		// 表外关键字会被当成未知字段拒收（400 Invalid JSON payload），
		// type 取值必须大写，也不接受联合 type 数组。
		//
		// 白名单而非黑名单：结构性关键字（$ref、$defs、oneOf、allOf、
		// prefixItems）漏一个，请求就原样发出去拿一个不可重试的 400，
		// 而 DroppedKeys 为空意味着连有损说明都报不出来。
		//
		// 表内没有 title：原来的黑名单剔除它且上线未见问题，而本协议是否
		// 真的接受 title 无实测也无官方明示——按保守维持剔除。
		// 表内没有 additionalProperties / patternProperties /
		// exclusiveMinimum / exclusiveMaximum / $schema / $id / deprecated：
		// 它们本来就在剔除之列，现在由白名单一并覆盖。
		SchemaDialect: schemadialect.Dialect{
			// 表内没有 minLength / maxLength / minItems / maxItems：
			// new-api 的白名单放行它们，而我们原来剔除且上线未见问题。
			// 两边冲突时不动已经跑通的行为——放行可能换来一个 400，
			// 而剔除只丢一条长度约束且已有有损说明。拿到能发请求的账号后
			// 再实测，别按参考实现改。
			Allow: []string{
				"anyOf", "default", "description", "enum", "example", "format",
				"items", "maxProperties", "maximum", "minProperties", "minimum",
				"nullable", "pattern", "properties", "propertyOrdering",
				"required", "type",
			},
			// enum 成员必须是字符串：{"type":"integer","enum":[1,2]} 会拿到
			// Invalid value at 'enum[0]' (TYPE_STRING)。
			StringEnumOnly:      true,
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
//
// 返回空串表示这个模型名拼不出一个安全的 URL（见 modelPathSegment），
// 由调用方把它变成一次可重试的失败：换目标会换 nativeModel。
func (outboundCodec) Endpoint(baseURL, nativeModel string, stream bool) (string, map[string]string) {
	seg, err := modelPathSegment(nativeModel)
	if err != nil {
		return "", nil
	}
	base := normalizeBaseURL(baseURL) + "/models/" + seg
	if stream {
		return base + ":streamGenerateContent?alt=sse", nil
	}
	return base + ":generateContent", nil
}

// normalizeBaseURL 去掉尾斜杠，并容忍配置里已经带上 /models 的 base_url。
//
// 只削一层：配成 /v1beta/models/models 的人要的就是那个路径，
// 循环削到没有会把一个可能合法的上游路径改掉。
func normalizeBaseURL(baseURL string) string {
	base := strings.TrimRight(baseURL, "/")
	return strings.TrimSuffix(base, "/models")
}

// modelPathSegment 把模型名变成一个安全的路径段。
//
// 转义而不是校验字符集：模型名的合法取值由上游定义，本服务列白名单只会在
// 上游上新模型时误拒。而 ? # 空格这些字符直接拼进 URL 会被 URL 解析吃掉——
// 探针实测 `m#x` 让整个 alt=sse 查询串消失，于是上游回整份 JSON 而不是 SSE，
// 三个同名目标会连挂三次。
//
// 但路径分隔语义必须拒绝而不是转义：PathEscape 不编码 . 与 /，
// 一个含 ../ 的模型名能把请求打到另一个端点上去。
func modelPathSegment(nativeModel string) (string, error) {
	if nativeModel == "" {
		return "", fmt.Errorf("empty model name")
	}
	// 斜杠一律拒绝而不是逐段检查 . 与 ..：Gemini 的模型名里从来没有斜杠，
	// 逐段放行等于替上游发明一套路径语法，而放行的每一段都得再回答一次
	// 「这一段会不会改变端点」。
	if strings.ContainsAny(nativeModel, `/\`) {
		return "", fmt.Errorf("model name contains a path separator: %q", nativeModel)
	}
	if nativeModel == "." || nativeModel == ".." {
		return "", fmt.Errorf("model name is a path traversal segment: %q", nativeModel)
	}
	return url.PathEscape(nativeModel), nil
}

func (outboundCodec) NewStreamDecoder() codec.StreamDecoder { return newStreamDecoder() }

func (outboundCodec) DecodeResponse(body []byte) (*ir.Response, error) { return DecodeResponse(body) }

// DecodeResponseLossy 实现 codec.LossyResponseDecoder：本协议的响应里有
// candidates 数组，且收尾原因可能附一段本服务装不下的人类可读说明。
func (outboundCodec) DecodeResponseLossy(body []byte) (*ir.Response, []string, error) {
	return DecodeResponseLossy(body)
}

func (outboundCodec) DecodeError(status int, header http.Header, body []byte) *ir.Error {
	return DecodeError(status, header, body)
}

func init() {
	codec.RegisterOutbound(outboundCodec{})
}
