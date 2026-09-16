// Package ir 是协议无关的中间表示。
//
// 所有入站请求先解码成 ir.Request，再由出站 codec 编码成上游协议的请求体；
// 响应严格反向。同协议（如 anthropic→anthropic）也走这条路径，不设透传快捷路径——
// 否则两条代码路径会各自漂移，同协议的 bug 无法被跨协议测试覆盖。
//
// IR 只承载协议共有语义。出站协议特有的调参字段（gemini 的 generationConfig、
// responses 的 reasoning.effort 等）不进 IR，由上游模型配置的 defaults/overrides
// 在编码出请求体之后作用上去。
package ir

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

type BlockType string

const (
	BlockText       BlockType = "text"
	BlockImage      BlockType = "image"
	BlockToolUse    BlockType = "tool_use"
	BlockToolResult BlockType = "tool_result"
	BlockThinking   BlockType = "thinking"
)

// Block 用判别式 Type 而非 interface：IR 要能原样序列化进日志与测试黄金文件，
// interface 反序列化需要自定义 UnmarshalJSON，得不偿失。
type Block struct {
	Type       BlockType   `json:"type"`
	Text       string      `json:"text,omitempty"`
	Image      *Image      `json:"image,omitempty"`
	ToolUse    *ToolUse    `json:"tool_use,omitempty"`
	ToolResult *ToolResult `json:"tool_result,omitempty"`
	Thinking   *Thinking   `json:"thinking,omitempty"`
	// CacheCtl 是 Anthropic 的 cache_control 类型（通常 "ephemeral"）。
	// 其他协议无此概念，编码时丢弃。
	CacheCtl string `json:"cache_ctl,omitempty"`
}

type Image struct {
	// MediaType 形如 "image/png"。Data 是 base64，URL 与 Data 二者其一。
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type ToolUse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Input 是工具入参的 JSON 对象。流式解码期间是逐片累积的不完整 JSON，
	// 只有 BlockStop 之后才保证可解析。
	Input string `json:"input,omitempty"`
}

type ToolResult struct {
	ToolUseID string  `json:"tool_use_id"`
	Content   []Block `json:"content,omitempty"`
	IsError   bool    `json:"is_error,omitempty"`
}

// Thinking 是推理内容。SignatureFrom 记录签名的来源协议，
// 因为签名只在同族协议间可透传：跨族（如 anthropic 的 signature 发给 responses）
// 上游会拒绝，必须丢弃。
type Thinking struct {
	Text          string `json:"text,omitempty"`
	Signature     string `json:"signature,omitempty"`
	SignatureFrom string `json:"signature_from,omitempty"`
}

type Message struct {
	Role    Role    `json:"role"`
	Content []Block `json:"content"`
}

type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Schema 是 JSON Schema 对象。各协议对它的嵌套位置不同，内容一致。
	Schema string `json:"schema,omitempty"`
}

type ToolChoiceMode string

const (
	ToolChoiceAuto ToolChoiceMode = "auto"
	ToolChoiceAny  ToolChoiceMode = "any"
	ToolChoiceNone ToolChoiceMode = "none"
	ToolChoiceTool ToolChoiceMode = "tool"
)

type ToolChoice struct {
	Mode ToolChoiceMode `json:"mode"`
	// Name 仅在 Mode 为 ToolChoiceTool 时有意义。
	Name string `json:"name,omitempty"`
}

// ThinkingConfig 同时容纳两种风格：Anthropic 用 token 预算，
// Responses/Gemini 用 effort 档位。哪个可用由出站 codec 决定，
// 无法映射的一侧留零值。
type ThinkingConfig struct {
	Enabled      bool   `json:"enabled"`
	Effort       string `json:"effort,omitempty"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type Request struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	// System 是系统提示。各协议承载位置不同（Anthropic 顶层 system、
	// Chat Completions 的 system 消息、Gemini 的 systemInstruction），
	// IR 统一放这里，由 codec 决定落点。
	System []Block `json:"system,omitempty"`

	Tools      []Tool      `json:"tools,omitempty"`
	ToolChoice *ToolChoice `json:"tool_choice,omitempty"`

	MaxTokens     int      `json:"max_tokens,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
	TopP          *float64 `json:"top_p,omitempty"`
	TopK          *int     `json:"top_k,omitempty"`
	StopSequences []string `json:"stop_sequences,omitempty"`
	Stream        bool     `json:"stream,omitempty"`

	Thinking *ThinkingConfig   `json:"thinking,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// Clone 深拷贝，供换目标重试时复用同一份原始请求。
// 每次尝试都要独立编码（native model 与参数覆盖不同），共享底层切片会串味。
func (r *Request) Clone() *Request {
	if r == nil {
		return nil
	}
	out := *r
	out.Messages = cloneMessages(r.Messages)
	out.System = cloneBlocks(r.System)
	out.Tools = append([]Tool(nil), r.Tools...)
	out.StopSequences = append([]string(nil), r.StopSequences...)
	if r.ToolChoice != nil {
		tc := *r.ToolChoice
		out.ToolChoice = &tc
	}
	if r.Temperature != nil {
		v := *r.Temperature
		out.Temperature = &v
	}
	if r.TopP != nil {
		v := *r.TopP
		out.TopP = &v
	}
	if r.TopK != nil {
		v := *r.TopK
		out.TopK = &v
	}
	if r.Thinking != nil {
		tc := *r.Thinking
		out.Thinking = &tc
	}
	if r.Metadata != nil {
		out.Metadata = make(map[string]string, len(r.Metadata))
		for k, v := range r.Metadata {
			out.Metadata[k] = v
		}
	}
	return &out
}

func cloneMessages(in []Message) []Message {
	if in == nil {
		return nil
	}
	out := make([]Message, len(in))
	for i, m := range in {
		out[i] = Message{Role: m.Role, Content: cloneBlocks(m.Content)}
	}
	return out
}

func cloneBlocks(in []Block) []Block {
	if in == nil {
		return nil
	}
	out := make([]Block, len(in))
	for i, b := range in {
		out[i] = b
		if b.Image != nil {
			v := *b.Image
			out[i].Image = &v
		}
		if b.ToolUse != nil {
			v := *b.ToolUse
			out[i].ToolUse = &v
		}
		if b.ToolResult != nil {
			v := *b.ToolResult
			v.Content = cloneBlocks(b.ToolResult.Content)
			out[i].ToolResult = &v
		}
		if b.Thinking != nil {
			v := *b.Thinking
			out[i].Thinking = &v
		}
	}
	return out
}
