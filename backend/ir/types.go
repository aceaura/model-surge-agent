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
	BlockText BlockType = "text"
	// BlockImage / BlockAudio / BlockDocument / BlockFile 是四类媒体块，
	// 共用 Block.Media 承载，Type 作判别位。BlockDocument 指模型能直读的
	// 文档（PDF 等），BlockFile 是其余附件——多数协议只能把后者降级为文本。
	BlockImage      BlockType = "image"
	BlockAudio      BlockType = "audio"
	BlockDocument   BlockType = "document"
	BlockFile       BlockType = "file"
	BlockToolUse    BlockType = "tool_use"
	BlockToolResult BlockType = "tool_result"
	BlockThinking   BlockType = "thinking"
)

// IsMedia 判断块是否由 Media 字段承载内容。
func (t BlockType) IsMedia() bool {
	switch t {
	case BlockImage, BlockAudio, BlockDocument, BlockFile:
		return true
	default:
		return false
	}
}

// Block 用判别式 Type 而非 interface：IR 要能原样序列化进日志与测试黄金文件，
// interface 反序列化需要自定义 UnmarshalJSON，得不偿失。
type Block struct {
	Type       BlockType   `json:"type"`
	Text       string      `json:"text,omitempty"`
	Media      *Media      `json:"media,omitempty"`
	ToolUse    *ToolUse    `json:"tool_use,omitempty"`
	ToolResult *ToolResult `json:"tool_result,omitempty"`
	Thinking   *Thinking   `json:"thinking,omitempty"`
	// CacheCtl 是 Anthropic 的 cache_control 类型（通常 "ephemeral"）。
	// 其他协议无此概念，编码时丢弃。
	CacheCtl string `json:"cache_ctl,omitempty"`
}

// Media 承载四类媒体块的内容。用一个结构而非每类各开一个字段：
// 四类的线上形态完全同形，分开只会多出四处 nil 检查。
type Media struct {
	// MediaType 形如 "image/png"、"audio/wav"、"application/pdf"。
	// Data 是 base64，URL 与 Data 二者其一。
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
	// Name 是附件文件名。只有部分协议表达得了，主要用于降级成文本时
	// 让模型知道这里本来有个什么文件。
	Name string `json:"name,omitempty"`
	// Detail 是图片的识别精度层级（"low"/"high"/"auto"），只有图片有，
	// 其余三类媒体恒为零值——同 Name 只对附件有意义。
	//
	// 零值表示客户端没给，此时出站不合成：合成一个会把「按上游默认」
	// 变成「按我们猜的」，而两者的计费可能不同。
	Detail string `json:"detail,omitempty"`
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
	// Redacted 标记载荷是不可解读的加密推理（Anthropic 的 redacted_thinking）。
	// 这类块出站时一律丢弃，留标记只为让有损诊断报得出来——
	// 解码时直接丢掉的话，IR 里就再没有痕迹可查。
	Redacted bool `json:"redacted,omitempty"`
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
	// ServerType 非空表示这是一个由上游自己执行的服务端工具
	// （anthropic 的 web_search_20250305、code_execution 之类），
	// 取值就是协议里的 type 原文。
	//
	// 用一个字段而不是另立类型：除了 type 这一处，服务端工具与函数工具
	// 在本服务眼里的处理完全相同（都要参与 tool_choice 校正、都要出现在
	// 工具集合里），分型会让每个遍历工具的地方都变成两个分支。
	//
	// 服务端工具不能被当成普通函数工具发出去：上游会把它当成「等客户端
	// 回结果」的函数，而本服务永远不会回——对话就停在那里，没有报错。
	ServerType string `json:"server_type,omitempty"`
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
	// Enabled 是三态：nil 表示客户端没提（随上游默认），false 表示客户端
	// 明确要求关闭，true 表示明确开启。两者必须分开：只有明确关闭才该在
	// 出站写出关闭标记，没提的那一档写出来会篡改上游默认。
	Enabled      *bool  `json:"enabled,omitempty"`
	Effort       string `json:"effort,omitempty"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

// On 判定明确开启。nil 接收者与 nil Enabled 都算「没明确开启」。
func (t *ThinkingConfig) On() bool {
	return t != nil && t.Enabled != nil && *t.Enabled
}

// Off 判定明确关闭。没提不算关闭。
func (t *ThinkingConfig) Off() bool {
	return t != nil && t.Enabled != nil && !*t.Enabled
}

// ThinkingOn / ThinkingOff 是构造三态取值的便捷函数。
func ThinkingOn() *bool  { v := true; return &v }
func ThinkingOff() *bool { v := false; return &v }

type Request struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	// System 是系统提示。各协议承载位置不同（Anthropic 顶层 system、
	// Chat Completions 的 system 消息、Gemini 的 systemInstruction），
	// IR 统一放这里，由 codec 决定落点。
	System []Block `json:"system,omitempty"`

	Tools      []Tool      `json:"tools,omitempty"`
	ToolChoice *ToolChoice `json:"tool_choice,omitempty"`

	// DecodeNotes 是入站解码阶段发现的、无法承载到 IR 的东西的说明，
	// 由 Sanitize 取走并并入它的返回值。
	//
	// 挂在请求上而不是改 Inbound.DecodeRequest 的签名：这类说明只有两个
	// 解码器会产生，改接口要动四个 codec 与全部调用点。零值即「没有」。
	DecodeNotes []string `json:"decode_notes,omitempty"`

	MaxTokens     int      `json:"max_tokens,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
	TopP          *float64 `json:"top_p,omitempty"`
	TopK          *int     `json:"top_k,omitempty"`
	StopSequences []string `json:"stop_sequences,omitempty"`
	Stream        bool     `json:"stream,omitempty"`

	Thinking *ThinkingConfig   `json:"thinking,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`

	// 以下调参字段全部用指针或空值表达「客户端没给」。不能用零值表达：
	// penalty 的 0 是「不惩罚」、seed 的 0 是一个具体种子、logprobs 的
	// false 是「明确不要」——都与「没提」不同，混起来就是替客户端表态。
	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
	Seed             *int     `json:"seed,omitempty"`
	// Candidates 是候选数（OpenAI 的 n、Gemini 的 candidateCount）。
	Candidates *int `json:"candidates,omitempty"`
	// LogProbs 请求对数概率；TopLogProbs 是每个 token 返回几个候选。
	LogProbs    *bool `json:"logprobs,omitempty"`
	TopLogProbs *int  `json:"top_logprobs,omitempty"`
	// LogitBias 是 token id → 偏置。跨模型不可翻译（词表不同），
	// 只在目标协议支持时原样透传，否则报丢弃。
	LogitBias map[string]float64 `json:"logit_bias,omitempty"`
	// ServiceTier 是计费与优先级档位。取值由各家定义，本服务不校验——
	// 上游是唯一知道哪些档位有效的一方。
	ServiceTier string `json:"service_tier,omitempty"`
	// ParallelToolCalls 是否允许一轮里并行多个工具调用。三态指针：
	// 没给就不替客户端表态（同 ThinkingConfig.Enabled 的判据）。
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`
	// ResponseFormat 是结构化输出要求。
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
	// Verbosity 是输出详略（responses 的 text.verbosity）。
	Verbosity string `json:"verbosity,omitempty"`
	// Include 要求上游额外返回哪些内容（responses 的 include）。
	Include []string `json:"include,omitempty"`
	// Truncation 是上游侧的历史截断策略（responses 的 truncation）。
	Truncation string `json:"truncation,omitempty"`
	// ClientMetadata 是客户端自定义元数据。与 Metadata 分开：后者只承载
	// user_id 且被翻译成各协议的用户标识字段，混在一起会让 user_id
	// 既作为用户标识、又作为一条普通元数据发出去两次。
	ClientMetadata map[string]string `json:"client_metadata,omitempty"`
}

// ResponseFormatKind 是结构化输出的形态。
type ResponseFormatKind string

const (
	// ResponseFormatJSON 要求输出是合法 JSON，不约束结构。
	ResponseFormatJSON ResponseFormatKind = "json"
	// ResponseFormatSchema 要求输出符合给定的 JSON Schema。
	ResponseFormatSchema ResponseFormatKind = "schema"
)

// ResponseFormat 是结构化输出要求。纯文本是默认形态，不进这个类型——
// 客户端没要求结构化时 Request.ResponseFormat 为 nil。
type ResponseFormat struct {
	Kind ResponseFormatKind `json:"kind"`
	// Name 与 Schema 仅在 Kind 为 ResponseFormatSchema 时有意义。
	Name   string `json:"name,omitempty"`
	Schema string `json:"schema,omitempty"`
	// Strict 要求上游严格遵循 schema。三态指针：各家默认值不同，
	// 客户端没表态时不替它选。
	Strict *bool `json:"strict,omitempty"`
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
	out.DecodeNotes = append([]string(nil), r.DecodeNotes...)
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
	out.PresencePenalty = cloneFloat(r.PresencePenalty)
	out.FrequencyPenalty = cloneFloat(r.FrequencyPenalty)
	out.Seed = cloneInt(r.Seed)
	out.Candidates = cloneInt(r.Candidates)
	out.TopLogProbs = cloneInt(r.TopLogProbs)
	out.LogProbs = cloneBool(r.LogProbs)
	out.ParallelToolCalls = cloneBool(r.ParallelToolCalls)
	if r.LogitBias != nil {
		out.LogitBias = make(map[string]float64, len(r.LogitBias))
		for k, v := range r.LogitBias {
			out.LogitBias[k] = v
		}
	}
	if r.ClientMetadata != nil {
		out.ClientMetadata = make(map[string]string, len(r.ClientMetadata))
		for k, v := range r.ClientMetadata {
			out.ClientMetadata[k] = v
		}
	}
	out.Include = append([]string(nil), r.Include...)
	if r.ResponseFormat != nil {
		rf := *r.ResponseFormat
		rf.Strict = cloneBool(r.ResponseFormat.Strict)
		out.ResponseFormat = &rf
	}
	return &out
}

func cloneFloat(p *float64) *float64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func cloneInt(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func cloneBool(p *bool) *bool {
	if p == nil {
		return nil
	}
	v := *p
	return &v
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
		if b.Media != nil {
			v := *b.Media
			out[i].Media = &v
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
