package anthropic

import "encoding/json"

// wireRequest 是 /v1/messages 的请求体。
type wireRequest struct {
	Model     string        `json:"model"`
	Messages  []wireMessage `json:"messages"`
	MaxTokens int           `json:"max_tokens"`
	// System 可以是字符串或块数组，两种都要认。
	System        json.RawMessage `json:"system,omitempty"`
	Tools         []wireTool      `json:"tools,omitempty"`
	ToolChoice    *wireToolChoice `json:"tool_choice,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	TopK          *int            `json:"top_k,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Thinking      *wireThinking   `json:"thinking,omitempty"`
	Metadata      *wireMetadata   `json:"metadata,omitempty"`
	// OutputConfig 2026 新增的输出控制：format 是结构化输出槽位。
	// effort 子字段暂不解码（IR 没有 anthropic 输出级 effort 的对应维度，
	// 那是 #28 思考现代化的事）。
	OutputConfig *wireOutputConfig `json:"output_config,omitempty"`
}

// wireOutputConfig 输出控制。format 只定义了 json_schema 一种 type：
// 本协议没有「只要求合法 JSON、不约束结构」那一档（Capabilities 里
// ResponseSchema 真而 ResponseFormat 假，纯 JSON 模式由诊断报受限）。
type wireOutputConfig struct {
	Format *wireJSONOutputFormat `json:"format,omitempty"`
}

type wireJSONOutputFormat struct {
	Type   string          `json:"type"` // 恒为 "json_schema"
	Schema json.RawMessage `json:"schema,omitempty"`
}

type wireMessage struct {
	Role string `json:"role"`
	// Content 可以是字符串或块数组。
	Content json.RawMessage `json:"content"`
}

type wireBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// Citations text 块的来源标注（托管搜索与文档引用都会下发）。用 RawMessage
	// 而不是 []citationIn：Anthropic 在 document / search_result 块上复用同一个
	// 键名承载 {"enabled":bool} 配置对象。声明成数组时那种块会让整条 content 的
	// json.Unmarshal 直接失败，同消息里的其他块（包括用户真正的问题）一起蒸发。
	Citations json.RawMessage `json:"citations,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`

	// image
	Source *wireSource `json:"source,omitempty"`

	// thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	// redacted_thinking 的载荷，本服务不解析只丢弃。
	Data string `json:"data,omitempty"`

	CacheControl *wireCacheControl `json:"cache_control,omitempty"`
}

// citationIn text 块 citations 数组元素的解码形状：官方 union 五种形态的字段并集。
// 只用来把可跨协议的字段投影进 IR；同族往返的保真靠 ir.Citation.Raw，不靠它。
type citationIn struct {
	Type           string `json:"type"`
	URL            string `json:"url"`
	Title          string `json:"title,omitempty"`
	CitedText      string `json:"cited_text,omitempty"`
	EncryptedIndex string `json:"encrypted_index,omitempty"`
	// 偏移量是 rune 下标，半开区间。不加 omitempty：0 是合法值。
	StartCharIndex int `json:"start_char_index"`
	EndCharIndex   int `json:"end_char_index"`
	// Source search_result_location 的来源 URL——该形态没有 url 键。
	Source string `json:"source,omitempty"`
	// DocumentTitle char/page/content_block 三种形态的文档标题——它们没有 title 键。
	DocumentTitle string `json:"document_title,omitempty"`
}

// citationOut 编码形状，严格照 web_search_result_location 的官方 schema：
// type / url / title / cited_text / encrypted_index。
//
// 刻意没有 start_char_index / end_char_index：那两个键属 char_location，
// 官方这一形态根本没有它们。按判别式校验的上游会把多出来的键当非法输入拒掉，
// 而客户端定位靠的是 cited_text，索引本来就用不上。
type citationOut struct {
	Type           string `json:"type"`
	URL            string `json:"url,omitempty"`
	Title          string `json:"title,omitempty"`
	CitedText      string `json:"cited_text"`
	EncryptedIndex string `json:"encrypted_index,omitempty"`
}

type wireSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type wireCacheControl struct {
	Type string `json:"type"`
}

type wireTool struct {
	// Type 空或 custom 表示普通函数工具，其余取值是上游自己执行的
	// 服务端工具（web_search_20250305 之类）。
	Type        string          `json:"type,omitempty"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

type wireToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

type wireThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type wireMetadata struct {
	UserID string `json:"user_id,omitempty"`
}

// wireResponse 是非流式响应体。
type wireResponse struct {
	ID           string      `json:"id"`
	Type         string      `json:"type"`
	Role         string      `json:"role"`
	Model        string      `json:"model"`
	Content      []wireBlock `json:"content"`
	StopReason   string      `json:"stop_reason,omitempty"`
	StopSequence string      `json:"stop_sequence,omitempty"`
	Usage        wireUsage   `json:"usage"`
}

// wireUsage 没有推理 token 维度：本协议把推理消耗直接算进 output_tokens。
// 转成本协议时该维度只是看不见，数值仍含在输出总量里，故不报有损。
type wireUsage struct {
	InputTokens              int64 `json:"input_tokens,omitempty"`
	OutputTokens             int64 `json:"output_tokens,omitempty"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens,omitempty"`
}

// SSE 事件名。
const (
	evMessageStart      = "message_start"
	evContentBlockStart = "content_block_start"
	evContentBlockDelta = "content_block_delta"
	evContentBlockStop  = "content_block_stop"
	evMessageDelta      = "message_delta"
	evMessageStop       = "message_stop"
	evPing              = "ping"
	evError             = "error"
)

// 块类型名。
const (
	blockText             = "text"
	blockImage            = "image"
	blockDocument         = "document"
	blockToolUse          = "tool_use"
	blockToolResult       = "tool_result"
	blockThinking         = "thinking"
	blockRedactedThinking = "redacted_thinking"
	// 服务端托管工具块：server_tool_use 复用 wireBlock 的 ID/Name/Input，
	// web_search_tool_result 复用 ToolUseID/Content（Content 是结果子块数组
	// 与错误对象的 union）。
	blockServerToolUse       = "server_tool_use"
	blockWebSearchToolResult = "web_search_tool_result"
)

// webSearchResultBlock web_search_tool_result.content 的结果子块形态。
// EncryptedContent 是原文摘要（上游侧加密，原样透传，非本服务加密）。
type webSearchResultBlock struct {
	Type             string `json:"type"` // "web_search_result"
	Title            string `json:"title"`
	URL              string `json:"url"`
	EncryptedContent string `json:"encrypted_content"`
	PageAge          string `json:"page_age,omitempty"`
}

// webSearchToolErrorBlock web_search_tool_result.content 的错误形态：
// content 是结果数组与本对象的 union，判别靠 error_code 非空。
type webSearchToolErrorBlock struct {
	Type      string `json:"type"` // "web_search_tool_result_error"
	ErrorCode string `json:"error_code"`
}

// streamEvent 是所有流帧的联合体。Anthropic 每种帧字段不同，
// 但字段名不冲突，用一个结构体解全部帧比每帧一个类型更短。
type streamEvent struct {
	Type    string       `json:"type"`
	Index   int          `json:"index,omitempty"`
	Message *streamMsg   `json:"message,omitempty"`
	Block   *wireBlock   `json:"content_block,omitempty"`
	Delta   *streamDelta `json:"delta,omitempty"`
	Usage   *wireUsage   `json:"usage,omitempty"`
	Error   *wireError   `json:"error,omitempty"`
}

type streamMsg struct {
	ID    string    `json:"id"`
	Model string    `json:"model"`
	Role  string    `json:"role"`
	Usage wireUsage `json:"usage"`
}

// streamDelta 既承载块内增量（text_delta 等），也承载 message_delta 的 stop_reason。
type streamDelta struct {
	Type        string `json:"type,omitempty"`
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	Signature   string `json:"signature,omitempty"`
	// Citation citations_delta 携带的单条引用原文。官方一帧一条，故不是数组。
	// 用 RawMessage 收：五种形态字段互不相同，逐字段建模会在解码这一步就把
	// 文档类引用的定位字段丢掉，编码回去只能凭空重建。
	Citation     json.RawMessage `json:"citation,omitempty"`
	StopReason   string          `json:"stop_reason,omitempty"`
	StopSequence string          `json:"stop_sequence,omitempty"`
}

// delta 类型名。
const (
	deltaText      = "text_delta"
	deltaInputJSON = "input_json_delta"
	deltaThinking  = "thinking_delta"
	deltaSignature = "signature_delta"
	// deltaCitations 每帧只带一条引用（上游的形状如此），多条时逐帧发送。
	deltaCitations = "citations_delta"
)

type wireError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type wireErrorEnvelope struct {
	Type  string    `json:"type"`
	Error wireError `json:"error"`
}
