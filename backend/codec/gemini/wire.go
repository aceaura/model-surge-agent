package gemini

import "encoding/json"

// wireRequest 是 generateContent 的请求体。
//
// 注意没有 model 字段：模型名在 URL 路径里。也没有 stream 字段：
// 流式由 :streamGenerateContent 方法名加 alt=sse 决定。
type wireRequest struct {
	Contents          []wireContent     `json:"contents"`
	SystemInstruction *wireContent      `json:"systemInstruction,omitempty"`
	Tools             []wireTools       `json:"tools,omitempty"`
	ToolConfig        *wireToolConfig   `json:"toolConfig,omitempty"`
	GenerationConfig  *wireGenerateCfg  `json:"generationConfig,omitempty"`
	SafetySettings    []wireSafetyEntry `json:"safetySettings,omitempty"`
}

// wireContent 的 Role 只有 user 与 model 两种，没有 system：
// 系统提示走 systemInstruction，工具结果算作 user 说的话。
type wireContent struct {
	Role  string     `json:"role,omitempty"`
	Parts []wirePart `json:"parts"`
}

// wirePart 是判别式联合体，但判别位不是一个 type 字段而是「哪个字段非空」。
// Thought 是布尔标记而非独立的 part 类型：带 thought:true 的 text part
// 就是推理内容。
type wirePart struct {
	Text             string            `json:"text,omitempty"`
	Thought          bool              `json:"thought,omitempty"`
	InlineData       *wireBlob         `json:"inlineData,omitempty"`
	FileData         *wireFileData     `json:"fileData,omitempty"`
	FunctionCall     *wireFunctionCall `json:"functionCall,omitempty"`
	FunctionResponse *wireFunctionResp `json:"functionResponse,omitempty"`
	// ThoughtSignature 是 gemini 的推理签名，只对本族有效。
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
}

type wireBlob struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type wireFileData struct {
	MimeType string `json:"mimeType,omitempty"`
	FileURI  string `json:"fileUri"`
}

// wireFunctionCall 没有调用 id，只有函数名。这是本协议与另外三个的关键差异：
// 工具结果靠函数名而非 id 回指，所以编码请求时要把 IR 的 id 翻回名字，
// 解码响应时要为调用合成一个 id。
type wireFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
	// ID 是较新版本可选返回的调用标识。有就用，没有才合成。
	ID string `json:"id,omitempty"`
}

type wireFunctionResp struct {
	Name string `json:"name"`
	// Response 必须是对象。纯文本结果要包一层。
	Response json.RawMessage `json:"response"`
	ID       string          `json:"id,omitempty"`
}

type wireTools struct {
	FunctionDeclarations []wireFunctionDecl `json:"functionDeclarations,omitempty"`
}

type wireFunctionDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type wireToolConfig struct {
	FunctionCallingConfig *wireFuncCallCfg `json:"functionCallingConfig,omitempty"`
}

type wireFuncCallCfg struct {
	// Mode 取 AUTO / ANY / NONE，大写是协议要求。
	Mode                 string   `json:"mode,omitempty"`
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

type wireGenerateCfg struct {
	MaxOutputTokens *int            `json:"maxOutputTokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"topP,omitempty"`
	TopK            *int            `json:"topK,omitempty"`
	StopSequences   []string        `json:"stopSequences,omitempty"`
	ThinkingConfig  *wireThinkinCfg `json:"thinkingConfig,omitempty"`
	// CandidateCount 对应 OpenAI 的 n。
	CandidateCount *int `json:"candidateCount,omitempty"`
	// ResponseLogprobs 是开关，Logprobs 是每 token 返回几个候选。
	// 本协议把两件事分成两个字段，与 chat_completions 的
	// logprobs / top_logprobs 一一对应。
	ResponseLogprobs *bool `json:"responseLogprobs,omitempty"`
	Logprobs         *int  `json:"logprobs,omitempty"`
	// ResponseMimeType 为 application/json 即要求 JSON 输出；
	// ResponseSchema 进一步约束结构，给了它就必须同时给 mimeType。
	ResponseMimeType string          `json:"responseMimeType,omitempty"`
	ResponseSchema   json.RawMessage `json:"responseSchema,omitempty"`
}

// wireThinkinCfg 的 IncludeThoughts 必须显式为真才能收到推理内容，
// 默认不返回。ThinkingBudget 用 token 数，与 Anthropic 的预算同量纲。
type wireThinkinCfg struct {
	IncludeThoughts bool `json:"includeThoughts,omitempty"`
	ThinkingBudget  *int `json:"thinkingBudget,omitempty"`
}

type wireSafetyEntry struct {
	Category  string `json:"category"`
	Threshold string `json:"threshold"`
}

// wireResponse 既是非流式响应体，也是流式的每一帧：
// alt=sse 下每帧都是一个完整的 GenerateContentResponse。
type wireResponse struct {
	Candidates     []wireCandidate `json:"candidates,omitempty"`
	UsageMetadata  *wireUsage      `json:"usageMetadata,omitempty"`
	ModelVersion   string          `json:"modelVersion,omitempty"`
	ResponseID     string          `json:"responseId,omitempty"`
	PromptFeedback *wireFeedback   `json:"promptFeedback,omitempty"`
	Error          *wireError      `json:"error,omitempty"`
}

type wireCandidate struct {
	Content      *wireContent `json:"content,omitempty"`
	FinishReason string       `json:"finishReason,omitempty"`
	Index        int          `json:"index,omitempty"`
	// FinishMessage 是上游随 finishReason 附的人类可读原因（比如具体
	// 触发了哪条安全策略）。IR 的 StopReason 是五个枚举之一，装不下它，
	// 所以只报说明、不改枚举——枚举已由 finishReason 决定，拿这里的
	// 文本去改会让两个来源打架。
	FinishMessage string `json:"finishMessage,omitempty"`
}

// wireFeedback 的 BlockReason 表示整个请求被安全策略拒了，
// 此时 candidates 为空，错误只能从这里读出。
type wireFeedback struct {
	BlockReason string `json:"blockReason,omitempty"`
}

type wireUsage struct {
	PromptTokenCount     int64 `json:"promptTokenCount,omitempty"`
	CandidatesTokenCount int64 `json:"candidatesTokenCount,omitempty"`
	CachedContentTokens  int64 `json:"cachedContentTokenCount,omitempty"`
	// ThoughtsTokenCount 是推理消耗，不含在 CandidatesTokenCount 里，
	// 计费上属于输出，累加进 output 才与账单一致。
	ThoughtsTokenCount int64 `json:"thoughtsTokenCount,omitempty"`
	TotalTokenCount    int64 `json:"totalTokenCount,omitempty"`
}

type wireError struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message"`
	Status  string `json:"status,omitempty"`
}

type wireErrorEnvelope struct {
	Error wireError `json:"error"`
}

// 角色名。本协议用 model 而非 assistant。
const (
	roleUser  = "user"
	roleModel = "model"
)

// 函数调用模式，大写是协议要求。
const (
	modeAuto = "AUTO"
	modeAny  = "ANY"
	modeNone = "NONE"
)
