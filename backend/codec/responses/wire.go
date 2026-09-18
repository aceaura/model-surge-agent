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

	// 以下三个字段把对话状态托管在上游那一侧，本服务表达不了：请求会被
	// 分发到任意一个目标账号，那里没有这条 id 指向的历史。收下再忽略等于
	// 悄悄丢掉客户端以为已经带上的上下文，只能显式拒收。
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	Conversation       json.RawMessage `json:"conversation,omitempty"`
	Prompt             json.RawMessage `json:"prompt,omitempty"`
}

type wireTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
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

	// function_call
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`

	// function_call_output
	Output string `json:"output,omitempty"`

	// reasoning
	Summary []wireSummary `json:"summary,omitempty"`
	// EncryptedContent 是加密的推理内容，本服务无法解读，只在同族协议间透传。
	EncryptedContent string `json:"encrypted_content,omitempty"`
}

type wireSummary struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type wirePart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// ImageURL 承载 data URI 或远程链接。
	ImageURL string `json:"image_url,omitempty"`
	// Refusal 是安全拒答文本，作为普通文本处理。
	Refusal string `json:"refusal,omitempty"`
	// InputAudio 的 format 是裸格式名（"wav"、"mp3"）而非完整 media type。
	InputAudio *wireInputAudio `json:"input_audio,omitempty"`
	// input_file 的三个字段是扁平的，不像音频那样嵌一层。
	Filename string `json:"filename,omitempty"`
	FileData string `json:"file_data,omitempty"`
	FileID   string `json:"file_id,omitempty"`
}

type wireInputAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

// wireResponse 是 response 对象，出现在非流式响应与流式的 response.* 帧里。
type wireResponse struct {
	ID     string     `json:"id,omitempty"`
	Object string     `json:"object,omitempty"`
	Model  string     `json:"model,omitempty"`
	Status string     `json:"status,omitempty"`
	Output []wireItem `json:"output,omitempty"`
	Usage  *wireUsage `json:"usage,omitempty"`
	// IncompleteDetails 的 reason 是本协议表达「因长度截断」的位置。
	IncompleteDetails *wireIncomplete `json:"incomplete_details,omitempty"`
	Error             *wireError      `json:"error,omitempty"`
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
	ContentIndex int       `json:"content_index,omitempty"`
	Item         *wireItem `json:"item,omitempty"`
	Part         *wirePart `json:"part,omitempty"`
	Delta        string    `json:"delta,omitempty"`
	// SummaryIndex 是推理摘要分段的序号。
	SummaryIndex int           `json:"summary_index,omitempty"`
	Response     *wireResponse `json:"response,omitempty"`
	// Code / Message 出现在 error 帧上（顶层而非嵌在 response 里）。
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
	evCreated              = "response.created"
	evInProgress           = "response.in_progress"
	evOutputItemAdded      = "response.output_item.added"
	evOutputItemDone       = "response.output_item.done"
	evContentPartAdded     = "response.content_part.added"
	evContentPartDone      = "response.content_part.done"
	evOutputTextDelta      = "response.output_text.delta"
	evOutputTextDone       = "response.output_text.done"
	evRefusalDelta         = "response.refusal.delta"
	evFunctionArgsDelta    = "response.function_call_arguments.delta"
	evFunctionArgsDone     = "response.function_call_arguments.done"
	evReasoningSummaryText = "response.reasoning_summary_text.delta"
	evReasoningTextDelta   = "response.reasoning_text.delta"
	evCompleted            = "response.completed"
	evIncomplete           = "response.incomplete"
	evFailed               = "response.failed"
	evError                = "error"
)

const (
	roleSystem    = "system"
	roleDeveloper = "developer"
	roleUser      = "user"
	roleAssistant = "assistant"
)
