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
	// OutputConfig 2026 新增的输出控制：format 是结构化输出槽位；
	// effort 子字段解码进 ir.Thinking.Effort（见 decode_request.go，
	// 独立出现也算开了思考），编码侧按封闭五值集校验后原值回写。
	OutputConfig *wireOutputConfig `json:"output_config,omitempty"`
	// OutputFormat beta 的请求级结构化输出旧槽位（官方 BetaJSONOutputFormatParam）：
	// 与 output_config.format 同形同判据，是同一诉求的废弃写法。只入不出——
	// 编码恒写新槽 output_config.format，不产出旧键。
	OutputFormat *wireJSONOutputFormat `json:"output_format,omitempty"`
	// CacheControl 顶层缓存便捷糖：自动给最后一个可缓存块打断点。
	// 不展开成块级，原样进出（理由见 ir.Request.TopCacheCtl）。
	CacheControl *wireCacheControl `json:"cache_control,omitempty"`
	// InferenceGeo 推理地理偏好（如 "us"）；缺省按 workspace 默认。
	InferenceGeo string `json:"inference_geo,omitempty"`
	// Container 代码执行容器复用标识与技能声明。官方两形态：string 简写
	// （仅 id）或 {id, skills} 对象——RawMessage 延迟判断。
	Container json.RawMessage `json:"container,omitempty"`
	// McpServers / ContextManagement 是 anthropic 的两个 beta 请求参数，本服务
	// 不解析、原文透传（同族回写兑现，跨族丢弃由诊断报出，理由见 ir.Request
	// 同名字段注释）。显式 null 在解码侧归一为没给。
	McpServers        json.RawMessage `json:"mcp_servers,omitempty"`
	ContextManagement json.RawMessage `json:"context_management,omitempty"`
	// Diagnostics 请求级诊断（官方 diagnostics={previous_message_id}）：原文透传，
	// 同族回写兑现、跨族丢弃由诊断报出（理由见 ir.Request.Diagnostics）。显式 null
	// 在解码侧归一为没给。
	Diagnostics json.RawMessage `json:"diagnostics,omitempty"`
	// ServiceTier 服务质量档位：auto / standard_only。OpenAI 方言值
	//（default/flex/...）由出站编码按 codec.MapServiceTier 翻译或丢弃。
	ServiceTier string `json:"service_tier,omitempty"`
}

// containerParams 请求侧 container 的对象形态（官方 ContainerParams）。
type containerParams struct {
	ID     string           `json:"id,omitempty"`
	Skills []containerSkill `json:"skills,omitempty"`
}

// container 响应侧容器回显（官方 Container：id/expires_at/skills 恒在，
// skills 可为 null）。请求侧技能 version 可缺省（=latest），响应侧必有值，
// 同形复用。
type container struct {
	ID        string           `json:"id"`
	ExpiresAt string           `json:"expires_at"`
	Skills    []containerSkill `json:"skills"`
}

type containerSkill struct {
	SkillID string `json:"skill_id"`
	Type    string `json:"type"` // "anthropic" / "custom"
	Version string `json:"version,omitempty"`
}

// wireOutputConfig 输出控制。format 只定义了 json_schema 一种 type：
// 本协议没有「只要求合法 JSON、不约束结构」那一档（Capabilities 里
// ResponseSchema 真而 ResponseFormat 假，纯 JSON 模式由诊断报受限）。
type wireOutputConfig struct {
	Format *wireJSONOutputFormat `json:"format,omitempty"`
	// Effort 思考档位（low/medium/high/xhigh/max，官方 OutputConfig.effort，
	// 是 OpenAI reasoning_effort 值集的子集——没有 none/minimal）。
	Effort string `json:"effort,omitempty"`
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
	// Context document 块的用途说明（官方 DocumentBlockParam.context）。与 text
	// 块的正文不是一回事：它是客户端给模型的旁注，不进 IR 的 Text，只落 Media.Context。
	Context string `json:"context,omitempty"`
	// Title document 块的标题（官方 DocumentBlock(Param).title，"The title of the
	// document"）。承载 IR Media.Name（附件文件名）：document 容器是 anthropic 唯一
	// 能表达附件名的槽位（image 块没有对应字段）。omitempty：异族投影来的附件或无
	// 文件名的文档给不出非空值，自然不写。
	Title string `json:"title,omitempty"`
	// Citations text 块的来源标注（托管搜索与文档引用都会下发）。用 RawMessage
	// 而不是 []citationIn：Anthropic 在 document / search_result 块上复用同一个
	// 键名承载 {"enabled":bool} 配置对象。声明成数组时那种块会让整条 content 的
	// json.Unmarshal 直接失败，同消息里的其他块（包括用户真正的问题）一起蒸发。
	Citations json.RawMessage `json:"citations,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// Caller / ToolsetName tool_use / server_tool_use 块的发起方标记与 beta
	// toolsets 归属名。Caller 官方是 union（DirectCaller | ServerToolCaller |
	// ServerToolCaller20260120）、响应侧必填、请求侧可选；用 RawMessage 逐字
	// 透传而不建模 union：本网关不据其分支，原样带回同族往返即无损。此前
	// tool_use / server_tool_use 作为已知块型逐字段重建，未建模的 caller /
	// toolset_name 会被静默丢掉（未知块型走 Raw 原文透传不受影响）。
	Caller      json.RawMessage `json:"caller,omitempty"`
	ToolsetName string          `json:"toolset_name,omitempty"`

	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`

	// FileID container_upload 块的文件引用（type=container_upload 时唯一载荷）。
	FileID string `json:"file_id,omitempty"`

	// image
	Source *wireSource `json:"source,omitempty"`
	// Transformations 是图片块的渲染指令（官方 image_block_param.transformations，
	// 目前只有 oversized_image 一维：图片超大时 downsize|error）。此前未建模，
	// 逐字段重建的 image 块会把它静默丢掉、同族往返不再逐字。
	Transformations *wireTransformations `json:"transformations,omitempty"`

	// thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	// redacted_thinking 的载荷，本服务不解析只丢弃。
	Data string `json:"data,omitempty"`

	CacheControl *wireCacheControl `json:"cache_control,omitempty"`

	// Raw 同族回写的不透明块原文（ir.BlockOpaque）。标 json:"-" 不参与逐字段
	// 序列化：MarshalJSON 见到它就把整块原样吐出去（与 wireTool.Raw 同一手法）。
	// 逐字段重建会丢掉 wireBlock 没建模的键，而这些块（web_fetch_tool_result 的
	// caller、code_execution_tool_result 的 stdout、search_result 的 source）的
	// 回传契约要求原样带回。
	Raw json.RawMessage `json:"-"`
}

// MarshalJSON 有原文的不透明块整块原样写出，其余按字段序列化。
func (b wireBlock) MarshalJSON() ([]byte, error) {
	if len(b.Raw) > 0 {
		return b.Raw, nil
	}
	type plain wireBlock
	return json.Marshal(plain(b))
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
	// FileID 是 type=file 源的上游文件服务引用（官方 FileImageSourceParam /
	// FileDocumentSourceParam：image_block 与 document_block 的 source union 都
	// 含 {type:"file",file_id}）。此前未建模 → 客户端发来的 file 源被
	// json.Unmarshal 静默吞掉、整块媒体变空壳，同族 anthropic→anthropic 往返
	// 整块蒸发。本服务不代取文件内容，只在同族逐字带回、跨族投给同样原生收
	// file_id 的目标（responses input_image/input_file）。
	FileID string `json:"file_id,omitempty"`
}

// wireTransformations 是图片块的渲染指令对象（官方 ImageTransformationsParam）。
// 目前官方只有 oversized_image 一维（"downsize"|"error"：图片超大时缩小还是报错）。
type wireTransformations struct {
	OversizedImage string `json:"oversized_image,omitempty"`
}

type wireCacheControl struct {
	Type string `json:"type"`
	// TTL 缓存存活档位（"5m"/"1h"，空=官方默认 5m）。
	TTL string `json:"ttl,omitempty"`
}

type wireTool struct {
	// Type 空或 custom 表示普通函数工具，其余取值是上游自己执行的
	// 服务端工具（web_search_20250305 之类）。
	Type        string          `json:"type,omitempty"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	// Strict 保证工具名与入参的 schema 校验（官方 Tool.strict）。
	Strict *bool `json:"strict,omitempty"`
	// CacheControl 工具定义上的缓存断点（官方 Tool.cache_control，
	// 含 ttl 档位）。丢了会让工具定义前缀每次重算重计费。
	CacheControl *wireCacheControl `json:"cache_control,omitempty"`
	// DeferLoading 不进初始 system prompt，由 tool search 按需加载。
	DeferLoading bool `json:"defer_loading,omitempty"`
	// EagerInputStreaming 细粒度流式入参开关（null=按 beta 头默认）。
	EagerInputStreaming *bool `json:"eager_input_streaming,omitempty"`
	// InputExamples 入参示例，不透明对象数组原文透传。
	InputExamples []json.RawMessage `json:"input_examples,omitempty"`
	// AllowedCallers 允许的程序化调用方（direct / code_execution_*）。
	AllowedCallers []string `json:"allowed_callers,omitempty"`
	// 以下四维是 web_search_* 服务端工具的声明参数（官方
	// WebSearchTool20250305）。函数工具上这些键不存在。
	MaxUses        int             `json:"max_uses,omitempty"`
	AllowedDomains []string        `json:"allowed_domains,omitempty"`
	BlockedDomains []string        `json:"blocked_domains,omitempty"`
	UserLocation   json.RawMessage `json:"user_location,omitempty"`
	// Raw 同族回写的服务端工具原始定义。标 json:"-" 不参与逐字段序列化：
	// MarshalJSON 见到它就把整块原样吐出去（与 citation.Raw 原文透传同一手法）。
	Raw json.RawMessage `json:"-"`
}

// MarshalJSON 有原文的服务端工具整块原样写出，其余按字段序列化。
// 逐字段重建会丢掉 wireTool 没建模的声明参数（computer 的 display_width_px/
// display_height_px、web_fetch 的 citations/max_content_tokens 及未来新增键）。
func (t wireTool) MarshalJSON() ([]byte, error) {
	if len(t.Raw) > 0 {
		return t.Raw, nil
	}
	type plain wireTool
	return json.Marshal(plain(t))
}

type wireToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
	// DisableParallelToolUse 禁止模型在一轮里并行调用多个工具（官方稳定字段，
	// 与 OpenAI 两系顶层的 parallel_tool_calls 同维反相）。三态指针：nil = 没提
	//（上游默认允许并行）；显式 true 才写。入站归一进 ir.Request.ParallelToolCalls，
	// 出站由 ParallelToolCalls=false 还原，同族原样往返、跨族与 parallel_tool_calls 互转。
	DisableParallelToolUse *bool `json:"disable_parallel_tool_use,omitempty"`
}

type wireThinking struct {
	Type         string `json:"type"` // "enabled" / "disabled" / "adaptive"
	BudgetTokens int    `json:"budget_tokens,omitempty"`
	// Display 思考内容回显形态（"summarized"=正常回显 / "omitted"=只回签名）。
	Display string `json:"display,omitempty"`
}

type wireMetadata struct {
	UserID string `json:"user_id,omitempty"`
}

// wireResponse 是非流式响应体。
type wireResponse struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Role  string `json:"role"`
	Model string `json:"model"`
	// Content 用 RawMessage 收，解码时逐块拆 raw：未知块型要整块留成不透明块
	// 供同族回吐，逐字段解析会丢掉没建模的键。一次性 []wireBlock 还会让任一块
	// 的形状冲突拖垮整个数组（见 decodeBlocks）。
	Content      json.RawMessage `json:"content"`
	StopReason   string          `json:"stop_reason,omitempty"`
	StopSequence string          `json:"stop_sequence,omitempty"`
	Usage        wireUsage       `json:"usage"`
	// StopDetails 拒绝档的结构化分类（官方 response.stop_details）。
	StopDetails *wireStopDetails `json:"stop_details,omitempty"`
	// Container 代码执行容器回显（按需出场，缺键与 null 同义）。
	Container *container `json:"container,omitempty"`
	// Diagnostics 请求级诊断回执（官方 Message.diagnostics={cache_miss_reason}）：
	// 原文透传，同族回写兑现、跨族丢弃由诊断报出（理由见 ir.Response.
	// AnthropicDiagnostics）。显式 null（未索要诊断或比对未完成）在解码侧归一为没给。
	Diagnostics json.RawMessage `json:"diagnostics,omitempty"`
}

// wireUsage 建模官方 Usage（message_start 与非流式响应的完整用量）。
// output_tokens 是含推理在内的权威计费总量；output_tokens_details.thinking_tokens
// 是只读的可观测分解（输出里多少是内部推理），不另计费。此前未建模该分解→
// anthropic 解码不填 IR.Usage.ReasoningTokens（chat/responses 都填了，唯 anthropic
// 漏），推理占比这一成本归因维度静默丢弃。总量始终保留，故只损可观测性、不损计费。
type wireUsage struct {
	// input_tokens / output_tokens 不带 omitempty：官方 Usage 模型里这两个键
	// 必填（SDK 反序列化按 required 校验），message_start 帧即使输出还没
	// 开始也必须带 output_tokens（值为 0 或上游的预估小值）。缺键会让严格
	// 客户端在流的第一帧就解析失败。
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens,omitempty"`
	// CacheCreation 写入用量的 TTL 明细。只在 IR 侧标记「明细已知」时写出；
	// 内层两键不带 omitempty——上游真回这个对象时两键总是同时出现，
	// 隐去零值反而会让读者以为明细残缺。
	CacheCreation *wireCacheCreationUsage `json:"cache_creation,omitempty"`
	// ServerToolUse 服务端托管工具执行次数（官方 usage.server_tool_use）。
	// 按次计费，看不见就无法对账托管搜索的成本。
	ServerToolUse *wireServerToolUsage `json:"server_tool_use,omitempty"`
	// OutputTokensDetails 输出 token 的只读分解（官方 usage.output_tokens_details，
	// 可显式 null）。thinking_tokens 已含在 output_tokens 内，单列只为成本归因。
	// 指针 + omitempty：缺席/null 解成 nil，编码侧仅在 IR.ReasoningTokens>0 时写出，
	// 异族来源给不出非零值自然不写（不伪造一个全零分解对象）。
	OutputTokensDetails *wireOutputTokensDetails `json:"output_tokens_details,omitempty"`
	// InferenceGeo 实际推理区域回显（官方 usage.inference_geo）。
	InferenceGeo string `json:"inference_geo,omitempty"`
	// ServiceTier 实际执行档位回显（standard/priority/batch）。**官方把它
	// 放在 usage 下**（Usage.service_tier），不是 Message 顶层——顶层没有
	// 这个键（据 anthropic-sdk-typescript：Message 仅 id/type/role/model/
	// content/stop_reason/stop_sequence/stop_details/container/usage）。
	// 只在 message_start 的完整 usage 与非流式响应里出现；message_delta 的
	// 精简 usage（MessageDeltaUsage）没有它。上游同族原值收下，跨族由编码器
	// 按 codec.MapServiceTierEcho 翻译或丢弃。
	ServiceTier string `json:"service_tier,omitempty"`
	// Iterations beta usage.iterations：按迭代阶段（message/compaction/advisor）
	// 细分的用量。判别式值域仍在演进，原文透传不建模；stable Usage 无此键，
	// 仅 beta 往返带得回。
	Iterations json.RawMessage `json:"iterations,omitempty"`
}

// wireOutputTokensDetails 是 output_tokens_details 的分解对象。内层键不带
// omitempty：官方回这个对象时 thinking_tokens 总在（required），且本编码器
// 仅在 ReasoningTokens>0 时才写出整个对象，不会出现「有对象、零分解」的残缺形。
type wireOutputTokensDetails struct {
	ThinkingTokens int64 `json:"thinking_tokens"`
}

// wireServerToolUsage 内层两键与 cache_creation 同理不带 omitempty：
// 官方回这个对象时两键总是成对出现，隐去零值会显得明细残缺。
type wireServerToolUsage struct {
	WebSearchRequests int64 `json:"web_search_requests"`
	WebFetchRequests  int64 `json:"web_fetch_requests"`
}

// wireCacheCreationUsage 是 cache_creation 的 TTL 细分对象：
// ephemeral_5m / ephemeral_1h 两档写入量，1h 档单价通常是 5m 的 2 倍。
type wireCacheCreationUsage struct {
	Ephemeral5mInputTokens int64 `json:"ephemeral_5m_input_tokens"`
	Ephemeral1hInputTokens int64 `json:"ephemeral_1h_input_tokens"`
}

// wireMessageDeltaUsage message_delta 帧的专用 usage（官方 MessageDeltaUsage）：
// 没有 cache_creation 对象、inference_geo、service_tier——那几个只属于 message_start
// 与非流式响应的完整 Usage。但官方 MessageDeltaUsage **有** output_tokens_details
// （与完整 Usage 同形），故这里也建模：同族「上游非流式→客户端流式」路径把聚合
// usage 整体落进 EvMessageDelta 时，推理分解要能随收尾帧送达。message_delta 复用
// 完整 wireUsage 会把官方 schema 没有的键写进帧里。解码侧仍按 wireUsage 宽松读：
// 官方 delta 帧的键是它的子集，多出来的 IR 维度保持零值。
type wireMessageDeltaUsage struct {
	InputTokens              int64                    `json:"input_tokens,omitempty"`
	OutputTokens             int64                    `json:"output_tokens"`
	CacheReadInputTokens     int64                    `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int64                    `json:"cache_creation_input_tokens,omitempty"`
	ServerToolUse            *wireServerToolUsage     `json:"server_tool_use,omitempty"`
	OutputTokensDetails      *wireOutputTokensDetails `json:"output_tokens_details,omitempty"`
	// Iterations 是上面「delta 帧不写完整 Usage 专属键」规矩的例外：官方
	// beta MessageDeltaUsage 与完整 usage 同形，也带 iterations。原文透传，
	// omitempty 保证非 beta 往返不会凭空写出。
	Iterations json.RawMessage `json:"iterations,omitempty"`
}

// messageDeltaEvent message_delta 帧的编码专用载荷：字段名与 streamEvent
// 相同，usage 换成官方的 delta 专用形状。
type messageDeltaEvent struct {
	Type  string                 `json:"type"`
	Delta *streamDelta           `json:"delta,omitempty"`
	Usage *wireMessageDeltaUsage `json:"usage,omitempty"`
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
	// container_upload 复用 wireBlock 的 FileID（该块唯一载荷）。
	blockContainerUpload = "container_upload"
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
	Type    string     `json:"type"`
	Index   int        `json:"index,omitempty"`
	Message *streamMsg `json:"message,omitempty"`
	// BlockRaw content_block_start 帧的块原文。用 RawMessage 而非 *wireBlock：
	// 未知块型要整块留成不透明块供同族回吐（见 decodeRawBlock），且块内某字段
	// 的形状冲突不该让整帧解析失败——那会把一个可透传的块误判成坏帧。
	BlockRaw json.RawMessage `json:"content_block,omitempty"`
	Delta    *streamDelta    `json:"delta,omitempty"`
	Usage    *wireUsage      `json:"usage,omitempty"`
	Error    *wireError      `json:"error,omitempty"`
}

type streamMsg struct {
	// Type/Content/StopReason/StopSequence 是官方 message_start 里 message
	// 对象的固定键集：type 恒为 "message"、content 恒为空数组、stop_reason
	// 与 stop_sequence 恒为显式 null（终止信息要等 message_delta 才有）。
	// 官方 SDK 按必填字段反序列化 Message 模型，缺 type 键会让严格客户端
	// 在流的第一帧直接解析失败——编码侧必须写全，解码侧收下忽略。
	Type    string            `json:"type,omitempty"`
	ID      string            `json:"id"`
	Model   string            `json:"model"`
	Role    string            `json:"role"`
	Content []json.RawMessage `json:"content"`
	// StopReason/StopSequence 用指针区分「显式 null」与「缺键」：
	// message_start 编码写显式 null，不带 omitempty。
	StopReason   *string   `json:"stop_reason"`
	StopSequence *string   `json:"stop_sequence"`
	Usage        wireUsage `json:"usage"`
	// Container 代码执行容器回显（message_start 首帧携带，缺键与 null 同义）。
	Container *container `json:"container,omitempty"`
	// Diagnostics 请求级诊断回执（官方 message_start 的完整 Message 上携带，
	// message_delta 的 Delta 无此字段）：原文透传，同族回写、跨族丢弃由诊断报出。
	Diagnostics json.RawMessage `json:"diagnostics,omitempty"`
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
	// StopDetails message_delta 上拒绝档的结构化分类（官方 Delta.stop_details）。
	StopDetails *wireStopDetails `json:"stop_details,omitempty"`
	// Container message_delta 上晚到的容器回显（官方 Delta.container）。
	Container *container `json:"container,omitempty"`
}

// wireStopDetails 拒绝的结构化信息（官方 RefusalStopDetails，type 恒
// "refusal"）。category/explanation 官方可显式 null，用 RawMessage 收：
// null 与缺省语义相同（官方注明），解码后同归空串，回写时空串省略。
type wireStopDetails struct {
	Type        string          `json:"type"`
	Category    json.RawMessage `json:"category,omitempty"`
	Explanation json.RawMessage `json:"explanation,omitempty"`
}

// delta 类型名。
const (
	deltaText      = "text_delta"
	deltaInputJSON = "input_json_delta"
	deltaThinking  = "thinking_delta"
	deltaSignature = "signature_delta"
	// deltaCitations 每帧只带一条引用（上游的形状如此），多条时逐帧发送。
	deltaCitations = "citations_delta"
	// deltaCompaction 服务端上下文压缩回执（官方 compaction_delta）：
	// encrypted_content 要求下一轮逐字回传，IR 没有槽位，解码分账计数。
	deltaCompaction = "compaction_delta"
)

type wireError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type wireErrorEnvelope struct {
	Type  string    `json:"type"`
	Error wireError `json:"error"`
}
