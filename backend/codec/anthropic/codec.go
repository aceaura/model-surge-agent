// Package anthropic 实现 Anthropic Messages 协议的入站与出站编解码。
//
// IR 的事件词汇取自本协议的流式模型（块生命周期 + 增量类型 + 独立 usage 帧），
// 因此这个包是 IR 的参照实现：另外三个协议的 codec 以「能否无损投影到这里」
// 为正确性判据。
package anthropic

import (
	"strings"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// Name 也用作 Thinking.SignatureFrom 的取值：签名只在同族协议间透传。
const Name = codec.ProtocolAnthropic

const apiVersion = "2023-06-01"

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
	// 本协议有 signature 字段，异族来源才丢。
	notes := codec.DescribeResponseSignatureLoss(resp, Name, true)
	// 本协议的响应信封没有执行档位的位置。上游报了就得说一声——
	// 这一维决定计费，无声丢掉会让客户端按点的档位对账。
	if resp != nil && resp.ServiceTier != "" {
		notes = append(notes, codec.DroppedServiceTierNote(Name))
	}
	return body, codec.DedupeNotes(notes), nil
}

func (inboundCodec) RenderError(err *ir.Error) (int, []byte) { return RenderError(err) }

func (inboundCodec) RenderErrorLossy(err *ir.Error) (int, []byte, []string) {
	return RenderErrorLossy(err)
}

func (inboundCodec) RenderStreamError(err *ir.Error) [][]byte { return RenderStreamError(err) }

type outboundCodec struct{}

func (outboundCodec) Name() string { return Name }

func (outboundCodec) Caps() codec.Capabilities {
	return codec.Capabilities{
		Thinking:      true,
		ThinkingSig:   true,
		Tools:         true,
		Images:        true,
		CacheControl:  true,
		TopK:          true,
		StopSequences: true,
		// 官方限定至多 4 个 cache_control 断点，超出即 400。
		CacheBreakpoints: 4,
		// 开启 thinking 时 temperature / top_p 必须缺席。
		ThinkingExcludesSampling: true,
		// 实测：thinking 开启时 tool_choice 为 any / 具名会被拒，上游原文是
		// tool_choice 'specified' is incompatible with thinking enabled。
		ThinkingExcludesForcedTools: true,
		// 预算低于 1024 会被拒；预算还必须小于 max_tokens。
		MinThinkingBudget: 1024,
		// 本协议的 max_tokens 必填，缺了直接 400。
		RequiresMaxTokens: true,
		// 4096 是个保守取值：宁可截断也不超出任何已知模型的输出上限。
		// 抬高它会在小窗口模型上变成不可重试的 400（max_tokens 超窗口即拒），
		// 而截断至少给出部分回答、且 stop_reason 说明了原因。
		// 兜底一旦发生会报一条有损诊断，客户端能看出这个上限不是它给的。
		DefaultMaxTokens: 4096,
		// 调参能力位全留假：本协议的请求体只有 model/messages/system/
		// max_tokens/metadata/stop_sequences/stream/temperature/top_k/top_p/
		// tools/tool_choice/thinking，没有承载 penalty、seed、n、logprobs、
		// logit_bias、service_tier、parallel_tool_calls、结构化输出的字段。
		// 这是照官方请求体核实的结果，不是没填。
		// SchemaDialect 留零值：本协议接受完整 JSON Schema。
		// 本协议只读图片与 PDF；音频与其他附件在编码时降级为文本。
		MediaTypes: []string{
			"image/png", "image/jpeg", "image/gif", "image/webp",
			"application/pdf",
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

// Endpoint 的 stream 参数在本协议下不影响路径：流式由请求体的 stream 字段决定。
//
// 额外头返回 nil：版本头曾在这里硬编，现已移交 DeclarationHeaders——
// 客户端声明的版本决定响应体形态，拿固定值覆盖它，客户端的解析器就对不上。
func (outboundCodec) Endpoint(baseURL, _ string, _ bool) (string, map[string]string) {
	return strings.TrimRight(baseURL, "/") + "/v1/messages", nil
}

// DeclarationHeaders 把客户端的协议声明落到本协议的头上。
//
// 不注入本服务自己的 beta 令牌：cc-switch 与 sub2api 都注入 Claude Code 的
// 令牌，目的是通过上游的「仅官方客户端」指纹检查。那是身份伪造，要做应当
// 由运维在调度层的账号头里配，不该由数据面代劳。
func (outboundCodec) DeclarationHeaders(d codec.Declarations) map[string]string {
	h := make(map[string]string, 2)
	if d.APIVersion != "" {
		h["anthropic-version"] = d.APIVersion
	} else {
		h["anthropic-version"] = apiVersion
	}
	if len(d.Betas) > 0 {
		h["anthropic-beta"] = strings.Join(d.Betas, ",")
	}
	return h
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
