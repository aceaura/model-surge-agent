// Package codec 定义协议编解码接口与注册表。
//
// 协议名与上游配置中心的 protocol 枚举一致，因此 dispatch 返回的
// target.protocol 可直接用来查出站 codec，无需映射表。
package codec

import (
	"net/http"
	"strings"

	"github.com/aceaura/model-surge-agent/backend/codec/schemadialect"
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

// StreamHeartbeat 是 StreamEncoder 的可选出口：静默期内给客户端发的保活帧。
//
// 挂在编码器上而不是由桥接层统一发一个注释帧：SSE 注释（`: \n\n`）对多数
// 客户端可行，但 Anthropic 协议有自己的 `ping` 事件类型，而严格按事件类型
// 分派的客户端遇到注释帧可能当成协议违规。形状只有编码器知道。
//
// 不实现该出口的协议由桥接层回落到注释帧。
type StreamHeartbeat interface {
	// HeartbeatFrame 返回一个不携带任何内容语义的帧。
	// 返回 nil 表示本协议不发保活。
	HeartbeatFrame() []byte
}

// HeartbeatComment 是没实现 StreamHeartbeat 的协议用的保活帧。
//
// SSE 注释行：规范要求客户端忽略以冒号开头的行，所以它不会被误当成数据。
var HeartbeatComment = []byte(": keepalive\n\n")

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
	// NewStreamEncoder 建这次请求的流式编码器。
	//
	// 带上请求而不是无参：客户端的部分表态只在流式编码时才用得到
	// （chat_completions 的 stream_options.include_usage 就是一例），
	// 编码器拿不到请求就只能按协议默认走，把明确的表态当没提。
	// req 可为 nil，表示没有请求上下文（各协议按默认形状编码）。
	NewStreamEncoder(req *ir.Request) StreamEncoder
	EncodeResponse(resp *ir.Response) ([]byte, error)
	// RenderError 编码非流式错误响应，返回 HTTP 状态码与响应体。
	RenderError(err *ir.Error) (int, []byte)
	// RenderStreamError 编码流内错误帧。此时 HTTP 200 已写出，状态码不可变，
	// 错误只能以客户端协议的流内错误形式表达。
	RenderStreamError(err *ir.Error) [][]byte
}

// Capabilities 声明出站协议能表达什么，供编码时决定丢弃哪些 IR 字段。
type Capabilities struct {
	Thinking    bool
	ThinkingSig bool
	// ToolCallSig 为真表示本协议的函数调用自身带推理签名字段
	// （gemini 的 functionCall part 上的 thoughtSignature）。只有 gemini 是这样。
	// 与 ThinkingSig 分开：两者是上游的不同状态，一个协议可以只有其中一个。
	ToolCallSig bool
	Tools       bool
	// ToolResultTextOnly 为真表示本协议的工具结果载荷只装文本。
	//
	// 与 Images 无关：这三个协议都能在普通消息里带图，只有工具结果这一处
	// 装不下（responses 的 function_call_output.output 与 gemini 的
	// functionResponse.response 都是单个字符串，chat_completions 的
	// role:tool 消息不接受媒体 part）。混用 Images 会让「能带图」与
	// 「工具结果里能带图」变成同一个判断，而它们不是。
	ToolResultTextOnly bool
	// ToolResultError 为真表示本协议的工具结果带失败标记（anthropic 的
	// is_error、gemini 的 error 键）。为假时失败态改写成内容前缀——
	// 丢掉它会让模型把失败当成功，那是跨轮语义被改坏且完全不可见。
	ToolResultError bool
	// ToolInputObject 为真表示本协议的工具调用参数槽是 JSON 对象形态
	// （anthropic 的 tool_use.input、gemini 的 functionCall.args）。
	// 畸形参数在对象槽位会被挪进 ir.RawArgsKey 键位保真，在字符串槽位
	// （chat 的 arguments、responses 的 arguments）原样透传。两者诊断
	// 措辞不同，读者要改的地方也不同。
	ToolInputObject bool
	// ServerTools 为真表示本协议表达得了「由上游自己执行的工具」。
	// 只有 anthropic 是这样。为假时这类声明整条丢弃并出说明，而不是
	// 降级成函数工具：降级后上游会等一个永远不来的工具结果。
	ServerTools bool
	// ToolStrict 为真表示工具定义有 strict 槽位（schema 严格校验保证）：
	// anthropic tool.strict、OpenAI 两系 function.strict，三族同义同形。
	// gemini 的工具定义没有这一维，装不下时报数不报值。
	ToolStrict    bool
	Images        bool
	CacheControl  bool
	TopK          bool
	StopSequences bool
	// 以下是调参字段的承载能力。为假时 DescribeLossy 报丢弃，
	// 请求照常发出——拒绝请求会把一个能用的回答换成零回答，而目标协议
	// 是调度层按策略选的，客户端无从预知，让它为此吃 400 归因方向是错的。
	Penalties  bool // presence_penalty / frequency_penalty
	Seed       bool
	Candidates bool // n / candidateCount
	LogProbs   bool
	// LogProbsViaTopN 为真表示本协议没有独立的 logprobs 开关，
	// top_logprobs 兼任开关与档位（responses 是这样）。此时客户端只给
	// logprobs 会什么也拿不到，出站须补一个档位——目标协议满足得了的
	// 请求不该因为字段形状不同而落空。
	LogProbsViaTopN bool
	// ImageDetail 为真表示本协议的图片块带 detail 层级
	// （chat_completions 与 responses 的 image_url/input_image）。
	// 为假时这一维丢弃并出说明：它决定计费与识别精度，客户端给过的
	// 东西悄悄没了会让账单对不上。
	ImageDetail bool
	// ImageFileRef 为真表示图片槽位可只凭上游文件服务的 id 投递
	// （Responses 的 input_image.file_id）。图片字节从未内联进请求体，
	// 本服务也不代取，所以这一维装不下就等于这张图彻底没了——与远程
	// URL 那种「换成 base64 即可」不同，读者无从补救，故单独立一位。
	// 当前只有 Responses 一族有这一维。
	ImageFileRef      bool
	LogitBias         bool
	ServiceTier       bool
	ParallelToolCalls bool
	// ResponseFormat 为真表示支持「输出必须是合法 JSON」这一档（纯 JSON 模式）；
	// ResponseSchema 为真表示支持按 JSON Schema 约束结构。
	//
	// 两位相互独立，不蕴含：anthropic 的 output_config.format 只有 json_schema
	// 一种 type（ResponseSchema 真而 ResponseFormat 假），纯 JSON 模式在它这里
	// 没有槽位；其余三家两位都真。分两位而不是用「schema 蕴含 JSON」的假设，
	// 正是为了让 anthropic 这个受限者能照实报出「只接 schema 约束形态」。
	ResponseFormat bool
	ResponseSchema bool
	Verbosity      bool
	Include        bool
	// Background 为真表示协议本身有「后台运行模式」槽位（responses 的
	// background）。注意这位不用于丢弃门控：本服务对上游一律流式请求且
	// store:false，而 background 要求非流式 + 服务端留存，因此即使同族也
	// 兑现不了，显式 true 一律报出（这位只区分注记措辞：有槽位而兑现不了，
	// 还是连槽位都没有）。其他三族连概念对应物都不存在。
	Background     bool
	Truncation     bool
	ClientMetadata bool

	// Citations 为真表示正文的来源标注有槽位。anthropic 是 text.citations，
	// chat_completions 是 message.annotations，responses 是
	// output_text.annotations；gemini 的 groundingMetadata 只在它的客户端
	// 方向存在，本服务对 gemini 只有出站请求侧，没有落点。装不下时正文
	// 照常送达，丢的是「这句话出自哪里」——客户端会把有出处的结论渲染成
	// 模型的自由发挥。
	Citations bool

	// Refusal 为真表示本协议有独立的「模型拒绝作答」槽位：chat 的
	// message.refusal、responses 的 refusal content part。anthropic 与
	// gemini 没有——那两家只有终止原因能表达「这是拒绝」，正文只能并入
	// 普通文本。装不下时降级为文本而非丢弃：拒绝正文是模型真正说出的话，
	// 丢了客户端只剩一条空消息配一个拒绝标记，像成功的空回复。
	Refusal bool

	// RequiresMaxTokens 为真表示本协议的输出上限必填，不能省略。
	RequiresMaxTokens bool
	// DefaultMaxTokens 是必填协议在客户端没给时的兜底值。
	//
	// 0 表示没有兜底值可用，此时编码必须失败而不是自己编一个数字：
	// 一个凭空的上限会在中途截断回答，而客户端从未设过它。这条路目前
	// 走不到（唯一 RequiresMaxTokens 的协议填了值），它守的是将来——
	// 谁加了新的必填协议却忘了给兜底值，会立刻失败而非静默发出 0。
	DefaultMaxTokens int

	// MediaTypes 是本协议接受的 media type 白名单。nil 表示只接受 image/*。
	// 白名单而非黑名单：上游对不认得的类型多回不可重试的 400，
	// 而不可重试意味着换目标也救不回来，只能在发出前降级。
	MediaTypes []string

	// SchemaDialect 描述本协议对工具 schema 的接受范围。零值表示全盘接受。
	SchemaDialect schemadialect.Dialect
	// CacheBreakpoints 是 cache_control 断点数量上限。
	// 0 表示不支持断点（由 CacheControl 位表达），负数表示无上限。
	CacheBreakpoints int
	// MaxStopSequences 是停止序列数量上限。0 表示无上限。
	MaxStopSequences int
	// MaxTemperature 是 temperature 的取值上限。0 表示不设限且跳过检查。
	//
	// 这是取值范围维度，与上面那些「能不能承载」的布尔位不同：字段能发，
	// 但值超出范围就是一个不可重试的 400（换目标也无用）。客户端按 OpenAI
	// 习惯发 temperature 1.5 是合法入站，被调度到 Anthropic 目标才出问题，
	// 而客户端无从预知目标协议是哪个——所以必须在出站侧夹紧。
	//
	// 当前只有 anthropic 填了 1.0：上游 400 原文为 temperature: range: 0..1。
	// 其余三个留零值——OpenAI 常说的 2.0 与 Gemini 各维上限都没有实测或
	// 官方明示的确证，猜出来的上限会把本来能过的请求改坏。
	MaxTemperature float64
	// ThinkingExcludesSampling 为真表示开启推理时不得同时发
	// temperature / top_p，同发会拿到不可重试的 400。
	ThinkingExcludesSampling bool
	// ThinkingExcludesForcedTools 为真表示开启推理时不得同时强制工具
	// （tool_choice 为 any / 具名），同发会拿到不可重试的 400。
	//
	// 冲突时关推理而不是降级工具约束：降级工具约束的故障不可见——上游会
	// 正常回一段文本，调用方以为模型选择了不调工具，而真相是约束被我们
	// 悄悄改掉了。关推理的损失是可见的，说明里写着，回答质量下降也能对上。
	ThinkingExcludesForcedTools bool
	// MinThinkingBudget 是推理预算的下限。0 表示无下限。
	// 预算还必须低于 max_tokens，两个约束在 max_tokens 过小时无解，
	// 此时只能关掉推理。
	MinThinkingBudget int
	// SystemAsText 为真表示本协议把系统提示承载为单一字符串
	// （responses 的 instructions、gemini 的 systemInstruction），
	// 因此 system 里的非文本块必须先降级成文本才不会丢。
	SystemAsText bool

	// ToolIDOptional 为真表示本协议的工具调用 id 字段可以缺席，
	// 上游靠调用顺序自行消歧。只有 gemini 是这样：它的 functionCall 与
	// functionResponse 靠 name 配对，id 是后来补的可选字段。
	//
	// 为真时，本服务自己合成的 id（见 SynthIDPrefix）不写进请求体：
	// 发一个上游从未见过的标识符回去，上游有权拒绝或错配。
	ToolIDOptional bool

	// MaxToolIDLen 是工具调用 id 的字节上限，0 表示不设限。
	//
	// 四协议当前全为 0：官方文档都没有明示上限，参考实现里也只有 Mistral
	// 有硬校验（^[a-zA-Z0-9]{9}$），而本服务没有这个出站。留着这个位是为了
	// 「某上游实测拒收长 id」时有地方落值，而不是现在就凭空猜一个——
	// 猜出来的上限会把本来能过的请求改坏。
	MaxToolIDLen int

	// MaxPayloadBytes 是出站请求体的字节上限，0 表示不设限且跳过测量。
	//
	// 同样全为 0：有实测证据的只有 Kiro（~615KB 起回一个 reason 为 null 的
	// 误导性 400），而 Kiro 的数据面归 Upstream 服务，不在本服务的出站协议里。
	MaxPayloadBytes int
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

// StreamNotes 是流式编解码器的可选出口，报告处理过程中改写或丢弃了什么。
//
// 编码器与解码器共用一个接口：两侧报的都是「客户端看到的内容与上游原样
// 有出入」，分两个同形接口只会让 pipeline 写两遍同样的收集代码。
//
// 做成可选接口而不是并入 StreamEncoder / StreamDecoder：后者是四个协议都要
// 实现的必经之路，加一个多数实现无话可说的方法只会逼出四个空壳。
//
// 说明累加而非覆盖：一个流里同一类丢弃会发生多次，且流式路径已 committed，
// 不存在「换目标重试」把前一次的说明作废的情形。返回值须去重排序。
type StreamNotes interface {
	Notes() []string
}

// LossyResponseEncoder 是非流式响应编码的可选出口，与 LossyEncoder 对称。
//
// 两条路径各要一个出口：非流式走 EncodeResponse 而不经流式编码器，
// 只给流式留出口会让非流式的丢弃继续无声。
type LossyResponseEncoder interface {
	// EncodeResponseLossy 除响应体外返回去重、已排序的有损说明。
	// 无丢弃时说明为 nil，且返回的响应体必须与 EncodeResponse 逐字节相同。
	EncodeResponseLossy(resp *ir.Response) ([]byte, []string, error)
}

// LossyResponseDecoder 是非流式响应解码的可选出口，与 LossyResponseEncoder
// 在出站方向的对称件。
//
// 解码同样会丢东西：上游回多路候选而中立表示只装得下一路，丢弃发生在
// DecodeResponse 内部，而它的签名里没有说明位。流式方向已有 StreamNotes，
// 非流式没有出口就会让同一类丢弃只在其中一条路径上可见。
//
// 只有响应里真有候选数组的协议实现它（chat_completions 与 gemini）。
type LossyResponseDecoder interface {
	// DecodeResponseLossy 除响应外返回去重、已排序的说明。
	// 无丢弃时说明为 nil，且返回的响应必须与 DecodeResponse 等价。
	DecodeResponseLossy(body []byte) (*ir.Response, []string, error)
}

// LossyErrorRenderer 是错误渲染的可选出口，报告错误的哪些维度因入站协议
// 表达不了而被丢掉。
//
// 只有真的丢维度的协议才实现它：anthropic 的错误信封只有 {type,message}
// 两个位，上游给的 param 到它这里无处安放。chat_completions 与 responses
// 都有 param 位，无话可说，不实现。
type LossyErrorRenderer interface {
	// RenderErrorLossy 除状态码与响应体外返回有损说明。
	// 无丢弃时说明为 nil，且返回的字节必须与 RenderError 逐字节相同。
	RenderErrorLossy(err *ir.Error) (int, []byte, []string)
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
	//
	// header 是上游的响应头，限流到期时刻只在头里（Retry-After、
	// anthropic-ratelimit-*、x-ratelimit-reset-* 等）。收进签名而不做成
	// 可选接口：限流头是 HTTP 层的，四个协议全都可能收到，漏一个就是缺口。
	DecodeError(status int, header http.Header, body []byte) *ir.Error
}
