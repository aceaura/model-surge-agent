package gemini

import (
	"encoding/json"
	"sort"
)

// wireRequest 是 generateContent 的请求体。
//
// 注意没有 model 字段：模型名在 URL 路径里。也没有 stream 字段：
// 流式由 :streamGenerateContent 方法名加 alt=sse 决定。
type wireRequest struct {
	Contents []wireContent `json:"contents"`
	// SystemInstruction 系统提示槽位；空 system 时省略键。
	SystemInstruction *wireContent     `json:"systemInstruction,omitempty"`
	Tools             []wireTools      `json:"tools,omitempty"`
	ToolConfig        *wireToolConfig  `json:"toolConfig,omitempty"`
	GenerationConfig  *wireGenerateCfg `json:"generationConfig,omitempty"`
	// 没有 safetySettings 槽位：IR 无安全阈值维度，入站三协议也没有
	// 对应参数，留着就是一个永远为空的死键。风控偏好属于调度层与
	// 上游账号配置，不经本服务转发。
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
	// unknownKeys 收下本结构没建模的 part 字段名（executableCode /
	// codeExecutionResult / videoMetadata / 以及未来新增的种类）。gemini 的
	// part 是「哪个字段非空」的判别式联合体，未建模的种类解完就凭空消失；
	// 记下键名，解码侧才能报出「丢了个未识别的 part」而不是静默跳过。
	unknownKeys []string `json:"-"`
}

// knownPartKeys 是 wirePart 已建模的字段名，用于从原始 JSON 里挑出未知键。
var knownPartKeys = map[string]bool{
	"text": true, "thought": true, "inlineData": true, "fileData": true,
	"functionCall": true, "functionResponse": true, "thoughtSignature": true,
}

// UnmarshalJSON 在按已建模字段解码之外，额外扫一遍顶层键挑出未建模的种类。
func (p *wirePart) UnmarshalJSON(data []byte) error {
	// 别名类型避免递归调用本方法。
	type plain wirePart
	var pl plain
	if err := json.Unmarshal(data, &pl); err != nil {
		return err
	}
	*p = wirePart(pl)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for k := range raw {
		if !knownPartKeys[k] {
			p.unknownKeys = append(p.unknownKeys, k)
		}
	}
	sort.Strings(p.unknownKeys)
	return nil
}

// hasUnknownContent 报告这个 part 是否只携带了未建模的字段——即本服务识别
// 不出任何已知内容，是个会被静默丢掉的未知 part 种类。带任一已知内容的 part
// 不算（它已被正常解码，未知键只是附带的次要信息）。
func (p wirePart) hasUnknownContent() bool {
	return len(p.unknownKeys) > 0 &&
		p.Text == "" && !p.Thought &&
		p.InlineData == nil && p.FileData == nil &&
		p.FunctionCall == nil && p.FunctionResponse == nil
}

type wireBlob struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
	// DisplayName 是官方 Blob.displayName（可选，"the name used to refer to this
	// blob to the model, e.g. my_blob.png"）。承载 IR Media.Name（附件文件名）：
	// chat 的 file.filename / responses 的 input_file.filename 由客户端设入，此前
	// gemini 出站整条丢弃、无注记——文件名对文档类附件是有语义的（模型据此区分
	// 多个附件）。omitempty：异族来源或无文件名的媒体给不出非空值，自然不写。
	DisplayName string `json:"displayName,omitempty"`
}

type wireFileData struct {
	MimeType string `json:"mimeType,omitempty"`
	FileURI  string `json:"fileUri"`
	// DisplayName 官方 FileData.displayName（可选，"the name used to refer to this
	// file to the model, e.g. my_file.pdf"），与 wireBlob 同维，承载 Media.Name。
	DisplayName string `json:"displayName,omitempty"`
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
	// Seed 与两个惩罚项与 OpenAI 同义、同量纲，只是键名是驼峰。
	Seed             *int     `json:"seed,omitempty"`
	PresencePenalty  *float64 `json:"presencePenalty,omitempty"`
	FrequencyPenalty *float64 `json:"frequencyPenalty,omitempty"`
}

// wireThinkinCfg 的 IncludeThoughts 必须显式为真才能收到推理内容，
// 默认不返回。ThinkingBudget 用 token 数，与 Anthropic 的预算同量纲。
type wireThinkinCfg struct {
	IncludeThoughts bool `json:"includeThoughts,omitempty"`
	ThinkingBudget  *int `json:"thinkingBudget,omitempty"`
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
	// CitationMetadata / GroundingMetadata 是 gemini 的来源标注：开了 Google
	// Search grounding（或代码引用）的上游会在候选上带出引用来源。此前二者都
	// 没建模→json.Unmarshal 静默吞掉，IR 的 Citation 槽位（chat 的 annotations、
	// anthropic 的 citations 都填它）在 gemini 上游这一路恒空、且无任何注记，
	// 与 wirePart 显式收未知键并报出的自我纪律相矛盾。只抽带 URI 的条目映进
	// ir.Citation（Portable 以 URL 为身份）；字节偏移的取舍见 candidateCitations。
	CitationMetadata  *wireCitationMetadata  `json:"citationMetadata,omitempty"`
	GroundingMetadata *wireGroundingMetadata `json:"groundingMetadata,omitempty"`
	// LogprobsResult 是上游按 responseLogprobs=true 计算出的逐 token 对数概率
	// （官方 Candidate.logprobsResult，Output only）。IR 响应模型没有逐 token
	// 概率槽位，只探测**真载荷**并计数报出——与 chat/responses 解码器同款载荷
	// 感知处置（LogProbsDropNote）。判据见 hasGeminiLogProbsPayload：logprobsResult
	// 是对象（topCandidates/chosenCandidates 数组 + logProbabilitySum 标量），空壳
	// 对象不算丢弃、不计数，避免朴素存在性判据的假阳性。不建模内部结构：只需知道
	// 「有没有真载荷」，不需解析内容。
	LogprobsResult json.RawMessage `json:"logprobsResult,omitempty"`
	// UrlContextMetadata 是 url_context 工具的检索确认（Output only）：上游
	// 成功取回的 URL 是来源归属（与 groundingChunks.web 同维），此前未建模→
	// json.Unmarshal 静默吞掉，与 Round 33 对 citationMetadata/groundingMetadata
	// 的保全纪律不一致。只采 SUCCESS/UNSPECIFIED 状态的 retrievedUrl 映进
	// ir.Citation；非成功状态（ERROR/PAYWALL/UNSAFE）的 URL 模型没用到，不是引用。
	UrlContextMetadata *wireUrlContextMetadata `json:"urlContextMetadata,omitempty"`
	// SafetyRatings 是上游对候选的按类别内容安全评级（官方 Candidate.safetyRatings，
	// Output only，「List of ratings for the safety of a response candidate」）。
	// 此前未建模→json.Unmarshal 静默吞掉，与同为 Output only 的 logprobsResult /
	// citationMetadata / groundingMetadata / urlContextMetadata 都已建模并报出的
	// 纪律不一致。IR 响应模型没有结构化安全评级槽位（gemini 形状与 chat/responses
	// 的 moderation 回执不同构，硬塞会让客户端误解析），故只探测计数经
	// SafetyRatingsDropNote 报出——含 finishReason=SAFETY 被拦时的类别/概率/拦截
	// 明细，也含正常完成时的信息性评级。
	SafetyRatings []wireSafetyRating `json:"safetyRatings,omitempty"`
}

// wireSafetyRating 是单条按类别的内容安全评级（官方 Candidate.safetyRatings[]
// 的元素，v1beta SafetyRating：category 危害类别 / probability 命中概率档 /
// blocked 是否因此拦截）。只用于探测存在性并计数，字段内容不进 IR。
type wireSafetyRating struct {
	Category    string `json:"category,omitempty"`
	Probability string `json:"probability,omitempty"`
	Blocked     bool   `json:"blocked,omitempty"`
}

// wireCitationMetadata 是候选级引用集合（官方 CitationMetadata.citationSources）。
type wireCitationMetadata struct {
	CitationSources []wireCitationSource `json:"citationSources,omitempty"`
}

// wireCitationSource 单条引用来源（官方 CitationSource：uri/startIndex/endIndex/
// license）。只 URI 进 IR；startIndex/endIndex 是相对候选全文的**字节**偏移，
// 与 IR 的 rune 口径不符，换算不可靠，故不携带（见 candidateCitations）。license
// 是来源的版权/许可标识，ir.Citation 无对应槽位、gemini 又是出站-only 跨族恒无维，
// 故探测计数经 CitationLicenseDropNote 报出（countCitationLicenses），不静默丢弃。
type wireCitationSource struct {
	URI        string `json:"uri,omitempty"`
	StartIndex int    `json:"startIndex,omitempty"`
	EndIndex   int    `json:"endIndex,omitempty"`
	License    string `json:"license,omitempty"`
}

// wireGroundingMetadata 是检索接地元数据。只取 groundingChunks 里带 URI 的
// 来源（web / retrievedContext）；searchEntryPoint、webSearchQueries、
// groundingSupports 等是展示/查询/分段侧信息，IR 无对应槽位、也不属于「正文
// 来源标注」的身份维，不在此保全。
type wireGroundingMetadata struct {
	GroundingChunks []wireGroundingChunk `json:"groundingChunks,omitempty"`
}

// wireGroundingChunk 一个接地来源块。web 是网页检索结果，retrievedContext 是
// 文件检索工具的结果，二者都带 URI（+标题）。
type wireGroundingChunk struct {
	Web              *wireWebSource    `json:"web,omitempty"`
	RetrievedContext *wireRetrievedCtx `json:"retrievedContext,omitempty"`
}

type wireWebSource struct {
	URI   string `json:"uri,omitempty"`
	Title string `json:"title,omitempty"`
}

type wireRetrievedCtx struct {
	URI   string `json:"uri,omitempty"`
	Title string `json:"title,omitempty"`
	Text  string `json:"text,omitempty"`
}

// wireUrlContextMetadata 是 url_context 工具的检索结果集合（官方
// UrlContextMetadata.urlMetadata[]）。只建模 retrievedUrl 与
// urlRetrievalStatus：前者是来源归属（映进 ir.Citation），后者门控
// 是否采集（非 SUCCESS 的 URL 模型没用到，不算引用）。
type wireUrlContextMetadata struct {
	UrlMetadata []wireUrlMetadata `json:"urlMetadata,omitempty"`
}

type wireUrlMetadata struct {
	RetrievedUrl       string `json:"retrievedUrl,omitempty"`
	UrlRetrievalStatus string `json:"urlRetrievalStatus,omitempty"`
}

// wireFeedback 的 BlockReason 表示整个请求被安全策略拒了，
// 此时 candidates 为空，错误只能从这里读出。
type wireFeedback struct {
	BlockReason string `json:"blockReason,omitempty"`
	// SafetyRatings 是上游对 **prompt 本身**（而非输出候选）算出的按类别内容
	// 安全评级（官方 PromptFeedback.safetyRatings：「Ratings for safety of the
	// prompt. There is at most one rating per category.」）。与 Candidate.safetyRatings
	// 同维（都是 wireSafetyRating 形状）但异源：那一条评的是模型输出、这一条评的是
	// 用户输入。此前未建模→promptFeedback 只读了 blockReason，整段 prompt 级评级被
	// json.Unmarshal 静默吞掉：prompt 被安全拦截时（candidates 为空）客户端只拿到一个
	// blockReason 枚举，看不到具体命中哪些危害类别、概率多强、是否因此拦截。IR 无结构
	// 化安全评级槽位（同 Candidate.safetyRatings 的取舍），故只探测计数，经 Notes()/
	// DecodeResponseLossy 用 **prompt 专属措辞** 报出（PromptSafetyRatingsDropNote），
	// 与候选级分账——共用候选措辞会把「评的是 prompt」误说成「评的是 candidate」。
	// 官方 blockReasonMessage 字段不在 v1beta discovery 文档里（未证实存在），不建模。
	SafetyRatings []wireSafetyRating `json:"safetyRatings,omitempty"`
}

type wireUsage struct {
	PromptTokenCount     int64 `json:"promptTokenCount,omitempty"`
	CandidatesTokenCount int64 `json:"candidatesTokenCount,omitempty"`
	CachedContentTokens  int64 `json:"cachedContentTokenCount,omitempty"`
	// ThoughtsTokenCount 是推理消耗，不含在 CandidatesTokenCount 里，
	// 计费上属于输出，累加进 output 才与账单一致。
	ThoughtsTokenCount int64 `json:"thoughtsTokenCount,omitempty"`
	// ToolUsePromptTokenCount 是生成函数调用参数的消耗。名字里带 Prompt 是
	// 上游的历史命名，语义上属于输出侧：它与 ThoughtsTokenCount 一样不含在
	// CandidatesTokenCount 里，而是 totalTokenCount 的一个独立加项。此前这一维
	// 全仓无人接收，工具调用回合的输出被系统性少计（少计的正是最贵的那部分）。
	ToolUsePromptTokenCount int64 `json:"toolUsePromptTokenCount,omitempty"`
	TotalTokenCount         int64 `json:"totalTokenCount,omitempty"`
	// ServiceTier 是上游回显的实际执行档位（Output only，enum
	// unspecified/standard/flex/priority）。此前未建模→gemini 上游时档位回声被
	// json.Unmarshal 静默吞掉，IR.Response.ServiceTier 恒空：客户端拿不到实际
	// 计费/优先级档位、也没有任何注记，与 anthropic/chat/responses 三族都捕获
	// tier echo 不对称。归一与透传见 decode_stream.go normalizeServiceTier。
	ServiceTier string `json:"serviceTier,omitempty"`
	// 以下五个 *TokensDetails 是官方 UsageMetadata 按模态（TEXT/IMAGE/AUDIO/
	// VIDEO）拆分的 token 明细，元素为 ModalityTokenCount{modality,tokenCount}。
	// 此前全未建模→多模态 gemini 上游返回时被 json.Unmarshal 静默吞掉，与已保全
	// 的 chat prompt_tokens_details.image_tokens/text_tokens（轮次41）同一类缺口。
	// 能归一进 IR 既有模态槽位的（输入 TEXT/IMAGE/AUDIO、输出 TEXT/AUDIO）保全；
	// IR 无槽位的（VIDEO 两侧、输出侧 IMAGE、缓存与工具用量的模态细分）计数后经
	// 注记报出，不静默丢弃。Gemini API 用 responseTokensDetails、Vertex 用
	// candidatesTokensDetails 指同一份输出明细，两者都收。
	PromptTokensDetails        []wireModalityTokenCount `json:"promptTokensDetails,omitempty"`
	CandidatesTokensDetails    []wireModalityTokenCount `json:"candidatesTokensDetails,omitempty"`
	ResponseTokensDetails      []wireModalityTokenCount `json:"responseTokensDetails,omitempty"`
	CacheTokensDetails         []wireModalityTokenCount `json:"cacheTokensDetails,omitempty"`
	ToolUsePromptTokensDetails []wireModalityTokenCount `json:"toolUsePromptTokensDetails,omitempty"`
}

// wireModalityTokenCount 是官方 ModalityTokenCount：一种模态消耗了多少 token。
// modality 取值 TEXT/IMAGE/AUDIO/VIDEO（及 MODALITY_UNSPECIFIED）。
type wireModalityTokenCount struct {
	Modality   string `json:"modality,omitempty"`
	TokenCount int64  `json:"tokenCount,omitempty"`
}

type wireError struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message"`
	Status  string `json:"status,omitempty"`
	// Details 是 google.rpc 的错误明细数组，按 @type 区分成员类型。
	// 唯一被采信的是 RetryInfo——它是本协议里唯一结构化的到期信息。
	Details []wireErrorDetail `json:"details,omitempty"`
}

// wireErrorDetail 只解出 RetryInfo 需要的两个键。
//
// 不解 ErrorInfo.reason（RATE_LIMIT_EXCEEDED vs MODEL_CAPACITY_EXHAUSTED）：
// 两者对本服务的处置相同——都是换目标 + 按到期时刻冷却，分开只会多一条
// 没有行为差异的分支。
type wireErrorDetail struct {
	Type string `json:"@type,omitempty"`
	// RetryDelay 是 Go duration 串，形如 "0.201506475s"。
	RetryDelay string `json:"retryDelay,omitempty"`
}

// retryInfoType 是 google.rpc.RetryInfo 的完整类型 URL。
//
// 全串比对而不是后缀匹配：details 数组里还有 ErrorInfo、QuotaFailure、
// BadRequest 等成员，宽松匹配会把别人的字段读成到期时刻。
const retryInfoType = "type.googleapis.com/google.rpc.RetryInfo"

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
