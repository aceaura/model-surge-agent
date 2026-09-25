package responses

import "encoding/json"

// wireRequest 是 /responses 的请求体。
//
// 本协议把消息与工具调用统一成 input 数组里的「条目」，条目类型比
// Chat Completions 的消息角色更细：function_call 与 function_call_output
// 是独立条目而非消息的字段。
type wireRequest struct {
	Model string `json:"model"`
	// Input 可以是字符串（等价于单条 user 消息）或条目数组。
	Input json.RawMessage `json:"input"`
	// Instructions 是本协议的系统提示位置。
	Instructions string `json:"instructions,omitempty"`
	// MaxOutputTokens 对应 IR 的 MaxTokens。
	MaxOutputTokens *int       `json:"max_output_tokens,omitempty"`
	Temperature     *float64   `json:"temperature,omitempty"`
	TopP            *float64   `json:"top_p,omitempty"`
	Stream          bool       `json:"stream,omitempty"`
	Tools           []wireTool `json:"tools,omitempty"`
	// ToolChoice 可以是字符串枚举或指定函数的对象。
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"`
	Reasoning  *wireReasoning  `json:"reasoning,omitempty"`
	// Store 控制上游是否留存对话。出站一律 false：本服务自己记流水，
	// 让上游留存会在多目标间产生互相看不见的分叉状态。
	Store *bool  `json:"store,omitempty"`
	User  string `json:"user,omitempty"`

	// 本协议特有的调参字段。Text 下嵌结构化输出与详略两项，
	// 与 Chat Completions 的顶层 response_format 不同位。
	Text    *wireText `json:"text,omitempty"`
	Include []string  `json:"include,omitempty"`
	// Background 后台运行模式。只进不出：解码收进 IR 供诊断报出，出站
	// 永不写键——本服务对上游一律 stream:true + store:false，与官方
	// background 的前置条件相反，写回去是保证被上游 400 的矛盾请求。
	Background *bool  `json:"background,omitempty"`
	Truncation string `json:"truncation,omitempty"`
	// MaxToolCalls 单轮响应允许的工具调用总上限。
	MaxToolCalls *int `json:"max_tool_calls,omitempty"`
	// StreamOptions 流式选项；本族目前只有 include_obfuscation。
	StreamOptions *wireStreamOptions `json:"stream_options,omitempty"`
	Metadata      map[string]string  `json:"metadata,omitempty"`
	ServiceTier   string             `json:"service_tier,omitempty"`
	// PromptCacheKey 提示缓存路由键。值是客户端自选串，日志与诊断不回显。
	PromptCacheKey    string `json:"prompt_cache_key,omitempty"`
	ParallelToolCalls *bool  `json:"parallel_tool_calls,omitempty"`
	TopLogProbs       *int   `json:"top_logprobs,omitempty"`
	// SafetyIdentifier 滥用检测标识，user 字段的官方替代。与 user 同一
	// 维度。值是用户标识，日志与诊断不回显。
	SafetyIdentifier string `json:"safety_identifier,omitempty"`
	// Moderation 请求级审核策略 {model, policy{input/output}}，原文透传。
	Moderation json.RawMessage `json:"moderation,omitempty"`
	// PromptCacheOptions 显式缓存断点控制 {mode, ttl, ...}，原文透传。
	PromptCacheOptions json.RawMessage `json:"prompt_cache_options,omitempty"`

	// 以下四个字段把对话状态托管在上游那一侧，本服务表达不了：请求会被
	// 分发到任意一个目标账号，那里没有这条 id 指向的历史。收下再忽略等于
	// 悄悄丢掉客户端以为已经带上的上下文，只能显式拒收。
	//
	// context_management 是上游自己裁剪历史的开关，同样依赖上游那一侧存着
	// 历史；收下再忽略会让客户端以为超长上下文已被裁剪，实际整段原样发出去
	// 并撞上窗口上限。
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	Conversation       json.RawMessage `json:"conversation,omitempty"`
	ContextManagement  json.RawMessage `json:"context_management,omitempty"`
	Prompt             json.RawMessage `json:"prompt,omitempty"`
}

// wireText 是本协议放输出形态与详略的位置。
type wireText struct {
	Format *wireTextFormat `json:"format,omitempty"`
	// Verbosity 取 low / medium / high。
	Verbosity string `json:"verbosity,omitempty"`
}

// wireTextFormat 的 type 取 text / json_object / json_schema。
// 与 Chat Completions 不同：schema 三项平铺在这一层，不再嵌一个 json_schema 对象。
type wireTextFormat struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
	// Description schema 的自然语言说明（与 chat 的 json_schema.description
	// 同键同义，位置平铺）。
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// wireStreamOptions Responses 的流式选项。IncludeObfuscation 三态指针：
// 显式 false 是「关掉上游默认开着的混淆保护」，与没提语义不同。
type wireStreamOptions struct {
	IncludeObfuscation *bool `json:"include_obfuscation,omitempty"`
}

type wireTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	// Strict 严格 schema 校验开关（官方 FunctionTool.strict）。
	Strict *bool `json:"strict,omitempty"`
}

type wireReasoning struct {
	Effort string `json:"effort,omitempty"`
	// Summary 控制是否返回推理摘要。本协议不返回原始思维链，
	// 只在开启 summary 时给出摘要文本。
	Summary string `json:"summary,omitempty"`
}

// wireItem 是 input 与 output 数组的元素。各类型共用一个结构体：
// 字段名不冲突，分开建类型要写四遍编解码。
type wireItem struct {
	Type string `json:"type,omitempty"`
	ID   string `json:"id,omitempty"`
	Role string `json:"role,omitempty"`
	// Content 可以是字符串或 part 数组。
	Content json.RawMessage `json:"content,omitempty"`
	Status  string          `json:"status,omitempty"`

	// function_call / custom_tool_call
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	// Input 是 custom_tool_call 条目的自由文本入参（与 function_call 的
	// arguments 互斥：自定义工具没有 JSON 参数概念）。
	Input string `json:"input,omitempty"`

	// function_call_output / custom_tool_call_output
	//
	// 指针而非字符串：output 是 Required 键（官方 response_input_item_param），
	// 空文本/纯媒体的工具结果也必须写 ""。string + omitempty 会把空串的键
	// 丢掉，编出 {"type":"function_call_output","call_id":...} 的非法形状。
	// nil 表示该条目类型本就没有这个键，照常缺席。
	//
	// RawMessage 是因为官方允许两种形态：字符串，或 content part 数组
	// （[{"type":"output_text",...},...]）。声明成字符串时数组形态会让
	// input 数组整段 Unmarshal 失败——合法请求被整单 400 拒掉。
	// 逐 part 解析在 decodeToolCallOutput 做；出站一律写回字符串形态。
	Output json.RawMessage `json:"output,omitempty"`

	// reasoning
	Summary []wireSummary `json:"summary,omitempty"`
	// EncryptedContent 是加密的推理内容，本服务无法解读，只在同族协议间透传。
	EncryptedContent string `json:"encrypted_content,omitempty"`
}

type wireSummary struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// wireRespItem 是响应侧专用的条目结构，零值有意义的字段不带 omitempty。
//
// 不能直接改 wireItem：那个结构同时服务 encode_request.go 的出站请求编码，
// 去掉 omitempty 会让请求体多出一批空字段，上游的 prompt cache 是按前缀
// 逐字节比对的，字节一变缓存就全部失效。
//
// Content 是 json.RawMessage，值为 nil 时即使没有 omitempty 也会写出
// "content":null，比字段缺席更糟——严格客户端把 null 当类型错误。
// 因此构造时必须显式赋 []，不能只靠去掉 tag。
type wireRespItem struct {
	Type   string `json:"type,omitempty"`
	ID     string `json:"id,omitempty"`
	Role   string `json:"role,omitempty"`
	Status string `json:"status,omitempty"`

	// message：客户端按下标往 content 里填 part，字段缺席时无处可填。
	Content json.RawMessage `json:"content,omitempty"`

	// function_call / custom_tool_call：客户端要按条目类型读全字段，
	// 空字符串表示「还没有入参」而不是「没有这个字段」，缺席会让客户端
	// 跳过该调用。必填只对这两类条目生效，见 MarshalJSON。
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	// Input 是 custom_tool_call 的自由文本入参终态，与 arguments 互斥。
	Input string `json:"input,omitempty"`

	// reasoning
	Summary          []wireSummary `json:"summary,omitempty"`
	EncryptedContent string        `json:"encrypted_content,omitempty"`
}

// MarshalJSON 在 function_call / custom_tool_call 条目上补出必填键。
//
// 按条目类型而非无条件必填：reasoning 与 message 条目带一个空 call_id
// 是本协议里不存在的形状，客户端 SDK 按 type 分派后读到不该有的字段会报错。
// 两类条目的必填集不同：function_call 是 call_id/name/arguments，
// custom_tool_call 是 call_id/name/input（自定义工具没有 JSON 参数概念）。
func (i wireRespItem) MarshalJSON() ([]byte, error) {
	type plain wireRespItem
	data, err := json.Marshal(plain(i))
	if err != nil {
		return nil, err
	}
	var required map[string]string
	switch i.Type {
	case itemFunctionCall:
		required = map[string]string{
			"call_id": i.CallID, "name": i.Name, "arguments": i.Arguments,
		}
	case itemCustomToolCall:
		required = map[string]string{
			"call_id": i.CallID, "name": i.Name, "input": i.Input,
		}
	default:
		return data, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, err
	}
	for key, val := range required {
		if _, ok := obj[key]; ok {
			continue
		}
		enc, err := json.Marshal(val)
		if err != nil {
			return nil, err
		}
		obj[key] = enc
	}
	return json.Marshal(obj)
}

type wirePart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// ImageURL 承载 data URI 或远程链接，双形态见 wireImageRef。
	ImageURL *wireImageRef `json:"image_url,omitempty"`
	// Detail 决定识别精度与计费档位，与 chat_completions 的
	// image_url.detail 同名同义。官方把它放在 part 顶层、与 image_url
	// 平级，不是嵌在 image_url 里——chat 形态才嵌。
	Detail string `json:"detail,omitempty"`
	// Refusal 是安全拒答文本，作为普通文本处理。
	Refusal string `json:"refusal,omitempty"`
	// InputAudio 的 format 是裸格式名（"wav"、"mp3"）而非完整 media type。
	InputAudio *wireInputAudio `json:"input_audio,omitempty"`
	// input_file 的三个字段是扁平的，不像音频那样嵌一层。
	// FileID 同时是 input_image 的第二种合法载体。
	Filename string `json:"filename,omitempty"`
	FileData string `json:"file_data,omitempty"`
	FileID   string `json:"file_id,omitempty"`
	// Annotations 是 output_text 的来源标注，随 part 给出终态快照；
	// 流式路径上的增量形态是 response.output_text.annotation.added 帧。
	Annotations []annotation `json:"annotations,omitempty"`
}

// annotation 是 url_citation 标注。字段是平的（chat 形态嵌一层
// url_citation 子对象，本族不嵌）。偏移量是 rune 下标、半开区间，
// 没有 omitempty：start=0 是合法取值。本协议没有 cited_text 字段。
type annotation struct {
	Type       string `json:"type"`
	URL        string `json:"url"`
	Title      string `json:"title,omitempty"`
	StartIndex int    `json:"start_index"`
	EndIndex   int    `json:"end_index"`
}

// wireImageRef 是 input_image 的图片载荷。
//
// 本族的规范形状是裸字符串（data URI 或远程 URL），但 chat 风格的对象
// {"url":…,"detail":…} 也会到这里来：sub2api 的 responses 桥对这两路都做了
// 分支，客户端确实混发。只认字符串时遇到对象会让整个 part 的 Unmarshal
// 失败：若它是消息里唯一的部件，图片连同所在消息一起消失。
// 出站一律写回裸字符串：本族上游只认这一种。
type wireImageRef struct {
	URL    string
	Detail string
}

func (r *wireImageRef) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		r.URL = s
		return nil
	}
	var o struct {
		URL    string `json:"url"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return err
	}
	r.URL, r.Detail = o.URL, o.Detail
	return nil
}

func (r wireImageRef) MarshalJSON() ([]byte, error) { return json.Marshal(r.URL) }

type wireInputAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

// wireResponse 是 response 对象，出现在非流式响应与流式的 response.* 帧里。
type wireResponse struct {
	ID     string `json:"id,omitempty"`
	Object string `json:"object,omitempty"`
	Model  string `json:"model,omitempty"`
	Status string `json:"status,omitempty"`
	// CreatedAt 是上游回显的创建时间（Unix 秒）。零值=上游没给，
	// 出站才回退本地钟（口径同 ir.Response.Created）。
	CreatedAt int64          `json:"created_at,omitempty"`
	Output    []wireRespItem `json:"output,omitempty"`
	Usage     *wireUsage     `json:"usage,omitempty"`
	// IncompleteDetails 的 reason 是本协议表达「因长度截断」的位置。
	IncompleteDetails *wireIncomplete `json:"incomplete_details,omitempty"`
	Error             *wireError      `json:"error,omitempty"`
	// ServiceTier 是上游实际执行的档位，随 response.created 与
	// response.completed 两帧各出现一次。
	ServiceTier string `json:"service_tier,omitempty"`
}

type wireIncomplete struct {
	Reason string `json:"reason,omitempty"`
}

type wireUsage struct {
	InputTokens         int64              `json:"input_tokens,omitempty"`
	OutputTokens        int64              `json:"output_tokens,omitempty"`
	TotalTokens         int64              `json:"total_tokens,omitempty"`
	InputTokensDetails  *wireInputDetails  `json:"input_tokens_details,omitempty"`
	OutputTokensDetails *wireOutputDetails `json:"output_tokens_details,omitempty"`
}

type wireInputDetails struct {
	CachedTokens int64 `json:"cached_tokens,omitempty"`
}

// wireOutputDetails 的 reasoning_tokens 已含在 output_tokens 内，
// 单列出来是为了看清推理占了多少。
type wireOutputDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
}

type wireError struct {
	Type    string `json:"type,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
	Param   string `json:"param,omitempty"`
}

type wireErrorEnvelope struct {
	Error wireError `json:"error"`
}

// wireStreamEvent 是所有 response.* 流帧的联合体。
type wireStreamEvent struct {
	Type string `json:"type"`
	// OutputIndex 是条目序号，充当 IR 的块索引来源。不保证连续。
	OutputIndex int `json:"output_index,omitempty"`
	// ContentIndex 是条目内 part 的序号；一个 message 条目可以有多个 part。
	ContentIndex int           `json:"content_index,omitempty"`
	Item         *wireRespItem `json:"item,omitempty"`
	Part         *wirePart     `json:"part,omitempty"`
	Delta        string        `json:"delta,omitempty"`
	// Arguments 是 function_call_arguments.done 携带的完整参数终态：
	// 不是新一份参数，已由 delta 交付的前缀不得重复。
	Arguments string `json:"arguments,omitempty"`
	// Input 是 custom_tool_call_input.done 携带的完整自由文本入参终态，
	// 口径同 Arguments。
	Input string `json:"input,omitempty"`
	// Text / Refusal 是 done 帧携带的完整终态值而不是新一份内容：
	// output_text.done 给 text，refusal.done 给 refusal，
	// reasoning_summary_text.done / reasoning_text.done 给 text。
	// 只发终态不发增量的上游全靠这两个字段，漏读就是整段正文静默丢失。
	Text    string `json:"text,omitempty"`
	Refusal string `json:"refusal,omitempty"`
	// Annotation 是 response.output_text.annotation.added 帧携带的单条
	// 来源标注。一帧一条，多条逐帧发送。
	Annotation *annotation `json:"annotation,omitempty"`
	// SummaryIndex 是推理摘要分段的序号。
	SummaryIndex int           `json:"summary_index,omitempty"`
	Response     *wireResponse `json:"response,omitempty"`
	// Error 是裸 error 事件携带的错误体。官方 wire 把它放在顶层
	// （{"type":"error","error":{...}}），不是 response.error 下；
	// 只读平铺三键会把上游给的 type/code/message 全部静默丢掉，
	// 客户端只看到一个空错误。Response.Error 仍作回落：response.failed
	// 走那条路径，平铺三键再作第三层（cc-switch / sub2api 同做多层回落）。
	Error *wireError `json:"error,omitempty"`
	// Code / Message 出现在部分网关平铺到帧顶层的错误帧上。
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Param   string `json:"param,omitempty"`
}

// 条目类型名。
const (
	itemMessage            = "message"
	itemFunctionCall       = "function_call"
	itemFunctionCallOutput = "function_call_output"
	itemReasoning          = "reasoning"
	// custom_tool_call 是自定义工具的调用条目：入参是自由文本而非 JSON，
	// 结果条目也独立成型（output 同样是自由文本）。
	itemCustomToolCall       = "custom_tool_call"
	itemCustomToolCallOutput = "custom_tool_call_output"
)

// part 类型名。input_ 前缀的用在请求，output_ 前缀的用在响应。
const (
	partInputText   = "input_text"
	partInputImage  = "input_image"
	partInputAudio  = "input_audio"
	partInputFile   = "input_file"
	partOutputText  = "output_text"
	partRefusal     = "refusal"
	partSummaryText = "summary_text"
)

// 流帧类型名。
const (
	evCreated          = "response.created"
	evInProgress       = "response.in_progress"
	evOutputItemAdded  = "response.output_item.added"
	evOutputItemDone   = "response.output_item.done"
	evContentPartAdded = "response.content_part.added"
	evContentPartDone  = "response.content_part.done"
	evOutputTextDelta  = "response.output_text.delta"
	evOutputTextDone   = "response.output_text.done"
	// evOutputTextAnnotationAdded 是正文来源标注的增量帧，一帧一条。
	evOutputTextAnnotationAdded = "response.output_text.annotation.added"
	evRefusalDelta              = "response.refusal.delta"
	evRefusalDone               = "response.refusal.done"
	evFunctionArgsDelta         = "response.function_call_arguments.delta"
	evFunctionArgsDone          = "response.function_call_arguments.done"
	// custom_tool_call 的入参增量走独立事件对，delta 键名是 input 而非
	// arguments，output_index 口径与 function_call 相同。
	evCustomToolInputDelta     = "response.custom_tool_call_input.delta"
	evCustomToolInputDone      = "response.custom_tool_call_input.done"
	evReasoningSummaryText     = "response.reasoning_summary_text.delta"
	evReasoningSummaryTextDone = "response.reasoning_summary_text.done"
	evReasoningSummaryPartDone = "response.reasoning_summary_part.done"
	evReasoningTextDelta       = "response.reasoning_text.delta"
	evReasoningTextDone        = "response.reasoning_text.done"
	evCompleted                = "response.completed"
	evIncomplete               = "response.incomplete"
	evFailed                   = "response.failed"
	evError                    = "error"
)

const (
	roleSystem    = "system"
	roleDeveloper = "developer"
	roleUser      = "user"
	roleAssistant = "assistant"
)

// requiredIndexFields 声明每个事件类型上「值为 0 也必须写出」的序号字段。
//
// 表驱动而非在各分支里逐个特判：新增事件类型时只改这一处，
// 漏填会被守卫测试发现，而散在分支里的补齐漏一处就无人察觉。
//
// error 与 response.created / in_progress / completed 一类生命周期帧
// 刻意不在表里：它们不属于任何条目，带上 output_index 会让客户端
// 把这些帧归到第一个条目上。
var requiredIndexFields = map[string][]string{
	evOutputItemAdded:  {"output_index"},
	evOutputItemDone:   {"output_index"},
	evContentPartAdded: {"output_index", "content_index"},
	evContentPartDone:  {"output_index", "content_index"},
	evOutputTextDelta:  {"output_index", "content_index"},
	evOutputTextDone:   {"output_index", "content_index"},
	// 第一条标注常在 output_index=0、content_index=0 上，两个零值都必须写出，
	// 否则客户端不知道这条标注属于哪个 part。
	evOutputTextAnnotationAdded: {"output_index", "content_index"},
	evRefusalDelta:              {"output_index", "content_index"},
	evRefusalDone:               {"output_index", "content_index"},
	evFunctionArgsDelta:         {"output_index"},
	evFunctionArgsDone:          {"output_index"},
	evCustomToolInputDelta:      {"output_index"},
	evCustomToolInputDone:       {"output_index"},
	evReasoningSummaryText:      {"output_index", "summary_index"},
	evReasoningSummaryTextDone:  {"output_index", "summary_index"},
	evReasoningSummaryPartDone:  {"output_index", "summary_index"},
	evReasoningTextDelta:        {"output_index"},
	evReasoningTextDone:         {"output_index"},
}
