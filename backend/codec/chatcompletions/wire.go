package chatcompletions

import "encoding/json"

// wireRequest 是 /chat/completions 的请求体。
type wireRequest struct {
	Model    string        `json:"model"`
	Messages []wireMessage `json:"messages"`
	// MaxTokens 与 MaxCompletionTokens 是同一语义的新旧写法，两个都要认。
	MaxTokens           *int     `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int     `json:"max_completion_tokens,omitempty"`
	Temperature         *float64 `json:"temperature,omitempty"`
	TopP                *float64 `json:"top_p,omitempty"`
	// Stop 可以是字符串或字符串数组。
	Stop          json.RawMessage    `json:"stop,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	StreamOptions *wireStreamOptions `json:"stream_options,omitempty"`
	Tools         []wireTool         `json:"tools,omitempty"`
	// ToolChoice 可以是字符串枚举或指定函数的对象。
	ToolChoice      json.RawMessage `json:"tool_choice,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	User            string          `json:"user,omitempty"`
}

// wireStreamOptions 的 include_usage 决定上游是否发 usage 帧。
// 出站一律置 true：用量要上报给调度层，缺了冷却与配额判断就失真。
type wireStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// wireMessage 同时用于请求消息、非流式响应的 message 与流式的 delta。
// 三处字段集合一致，分开建类型只会让编解码各写三遍。
type wireMessage struct {
	Role string `json:"role,omitempty"`
	// Content 可以是字符串、parts 数组或 null。
	Content json.RawMessage `json:"content,omitempty"`
	// ReasoningContent 是各家推理模型放思维链的位置，非 OpenAI 官方字段但已成事实标准。
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
	Name             string         `json:"name,omitempty"`
}

type wirePart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *wireImageURL `json:"image_url,omitempty"`
}

type wireImageURL struct {
	URL string `json:"url"`
}

// wireToolCall 的 Index 只在流式增量里出现，且必须写出：
// 客户端靠它把分片的 arguments 拼回对应的调用，缺了会导致回合中断。
type wireToolCall struct {
	Index    *int             `json:"index,omitempty"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function wireFunctionCall `json:"function"`
}

type wireFunctionCall struct {
	Name string `json:"name,omitempty"`
	// Arguments 是 JSON 字符串（不是对象）。流式期间逐片累积。
	Arguments string `json:"arguments,omitempty"`
}

type wireTool struct {
	Type     string          `json:"type"`
	Function wireFunctionDef `json:"function"`
}

type wireFunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type wireToolChoiceObject struct {
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

// wireResponse 同时解非流式响应与流式 chunk：前者填 Choices[].Message，
// 后者填 Choices[].Delta，其余字段一致。
type wireResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object,omitempty"`
	Created int64        `json:"created,omitempty"`
	Model   string       `json:"model,omitempty"`
	Choices []wireChoice `json:"choices"`
	Usage   *wireUsage   `json:"usage,omitempty"`
}

type wireChoice struct {
	Index        int          `json:"index"`
	Message      *wireMessage `json:"message,omitempty"`
	Delta        *wireMessage `json:"delta,omitempty"`
	FinishReason string       `json:"finish_reason,omitempty"`
}

type wireUsage struct {
	PromptTokens     int64 `json:"prompt_tokens,omitempty"`
	CompletionTokens int64 `json:"completion_tokens,omitempty"`
	TotalTokens      int64 `json:"total_tokens,omitempty"`
	// PromptTokensDetails 是 OpenAI 报缓存命中的位置。
	PromptTokensDetails *wirePromptDetails `json:"prompt_tokens_details,omitempty"`
	// PromptCacheHitTokens 是 DeepSeek 的写法，与上面同义，取其一即可。
	PromptCacheHitTokens int64 `json:"prompt_cache_hit_tokens,omitempty"`
	// CacheReadInputTokens 是照搬 Anthropic 命名的兼容层写法，同为缓存读取量。
	CacheReadInputTokens int64 `json:"cache_read_input_tokens,omitempty"`
	// 缓存写入量在本协议里没有官方字段，两个别名都是兼容层自造的。
	CacheWriteTokens    int64 `json:"cache_write_tokens,omitempty"`
	CacheCreationTokens int64 `json:"cache_creation_tokens,omitempty"`
}

type wirePromptDetails struct {
	CachedTokens int64 `json:"cached_tokens,omitempty"`
}

type wireError struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	// Code 各家有时是字符串有时是数字，用 RawMessage 兜住再取文本。
	Code  json.RawMessage `json:"code,omitempty"`
	Param string          `json:"param,omitempty"`
}

type wireErrorEnvelope struct {
	Error wireError `json:"error"`
}

// 角色名。developer 是 system 的新写法，解码时等同处理。
const (
	roleSystem    = "system"
	roleDeveloper = "developer"
	roleUser      = "user"
	roleAssistant = "assistant"
	roleTool      = "tool"
)

const (
	partText     = "text"
	partImageURL = "image_url"
)

// doneSentinel 是本协议的流终止标记，不是 JSON。
const doneSentinel = "[DONE]"

const chunkObject = "chat.completion.chunk"
