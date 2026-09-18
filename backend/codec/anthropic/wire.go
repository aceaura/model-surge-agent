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
}

type wireMessage struct {
	Role string `json:"role"`
	// Content 可以是字符串或块数组。
	Content json.RawMessage `json:"content"`
}

type wireBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`

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
	ID         string      `json:"id"`
	Type       string      `json:"type"`
	Role       string      `json:"role"`
	Model      string      `json:"model"`
	Content    []wireBlock `json:"content"`
	StopReason string      `json:"stop_reason,omitempty"`
	Usage      wireUsage   `json:"usage"`
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
)

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
	StopReason  string `json:"stop_reason,omitempty"`
}

// delta 类型名。
const (
	deltaText      = "text_delta"
	deltaInputJSON = "input_json_delta"
	deltaThinking  = "thinking_delta"
	deltaSignature = "signature_delta"
)

type wireError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type wireErrorEnvelope struct {
	Type  string    `json:"type"`
	Error wireError `json:"error"`
}
