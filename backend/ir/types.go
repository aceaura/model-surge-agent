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

import "encoding/json"

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
	// BlockServerToolUse / BlockWebSearchToolResult 是上游自己执行的服务端
	// 托管工具块（Anthropic 的 web_search 一族）：调用与结果都发生在上游，
	// 客户端从不回结果。与 BlockToolUse/BlockToolResult 分开建模是因为
	// 配平契约相反——普通工具调用缺结果会让上游拒整轮，托管工具没有
	// 「客户端欠一个结果」的概念；外族协议也没有对应槽位，只能整块跳过。
	// 不进 IR 会让 anthropic 上游真用 web_search 时整流报 unknown block，
	// 多轮历史里带这类块的同族往返直接 400。
	BlockServerToolUse       BlockType = "server_tool_use"
	BlockWebSearchToolResult BlockType = "web_search_tool_result"
	// BlockContainerUpload 容器文件引用块（Anthropic 的 container_upload）：
	// 请求侧把已上传的文件送进代码执行容器的输入目录，响应侧是模型运行代码后
	// 产出的文件引用。与四类媒体块分开建模是因为它没有内容本体——只有一个
	// file_id 与「进容器」的语义，塞进 Media.FileID 会让外族把它当普通附件
	// 投递，上游按内容解码后 400。外族没有容器概念，整块跳过并计损耗。
	BlockContainerUpload BlockType = "container_upload"
	// BlockRefusal 模型拒绝作答的正文。文本放 Text 字段。
	// 与 BlockText 分开是因为 OpenAI 两系有独立槽位（chat 的 message.refusal、
	// responses 的 refusal content part），而 anthropic 与 gemini 没有——合进
	// BlockText 会让同协议往返把拒绝降级成普通回答，客户端无法区分「模型拒绝了」
	// 和「模型这么答的」。只靠终止原因也不够：正文若丢，客户端看到的是一条
	// 空消息配一个拒绝标记，像成功的空回复。无槽位协议降级为文本而非丢弃，
	// 且不加标注前缀——正文会成为模型后续轮次读到的自己说过的话。
	BlockRefusal BlockType = "refusal"
)

// IsServerTool 判断块是否为服务端托管工具产物（调用或结果）。
// 外族编码器靠它整块跳过：托管工具的查询串没有本族槽位，
// 落进正文或工具调用槽都是伪造。
func (t BlockType) IsServerTool() bool {
	switch t {
	case BlockServerToolUse, BlockWebSearchToolResult:
		return true
	default:
		return false
	}
}

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
	// ServerToolUse / WebSearchToolResult 承载服务端托管工具块，
	// 仅 Anthropic 一族可往返，外族编码整块跳过（见 IsServerTool）。
	ServerToolUse       *ServerToolUse       `json:"server_tool_use,omitempty"`
	WebSearchToolResult *WebSearchToolResult `json:"web_search_tool_result,omitempty"`
	// ContainerUpload 承载容器文件引用块（BlockContainerUpload），仅
	// Anthropic 一族可往返，外族编码整块跳过（无 file_id 槽位）。
	ContainerUpload *ContainerUploadRef `json:"container_upload,omitempty"`
	// Citations 本块正文引用的来源。挂在块上而非消息上，是因为各协议都把它
	// 绑到单个文本块：Anthropic 的 text.citations、Chat 的 message.annotations、
	// Responses 的 output_text.annotations。偏移量也只有在单块正文内才有意义
	// ——跨块累加会在块被重排或降级时全部错位。
	Citations []Citation `json:"citations,omitempty"`
	// CacheCtl 是 Anthropic 的 cache_control 类型（通常 "ephemeral"）。
	// 其他协议无此概念，编码时丢弃并由诊断报出。
	CacheCtl string `json:"cache_ctl,omitempty"`
	// CacheTTL 缓存断点的存活档位（"5m"/"1h"，空=官方默认 5m）。仅
	// Anthropic 方向保留；与 CacheCtl 并列而非合并进字符串，是因为
	// type 与 ttl 是 cache_control 对象里两个独立键。丢了 ttl 会让 1h
	// 断点静默降级成 5m——计费与命中率都变。
	CacheTTL string `json:"cache_ttl,omitempty"`
}

// Container 代码执行容器的标识与技能声明（仅 Anthropic 一族）。
// 请求侧：ID 是跨请求复用的容器标识，Skills 是要加载的技能（Version 缺省
// 为 latest）；响应侧：ID/ExpiresAt 是实际使用的容器与过期时间，Skills 是
// 已加载技能（Version 必有值）。两侧共用一个类型，请求侧 ExpiresAt 恒空。
type Container struct {
	ID        string  `json:"id,omitempty"`
	ExpiresAt string  `json:"expires_at,omitempty"`
	Skills    []Skill `json:"skills,omitempty"`
}

// Skill 容器技能声明。Type 是 "anthropic"（内置）或 "custom"（用户自定义）。
type Skill struct {
	SkillID string `json:"skill_id,omitempty"`
	Type    string `json:"type,omitempty"`
	Version string `json:"version,omitempty"`
}

// ContainerUploadRef 容器文件引用（container_upload 块的载荷）。只有 file_id：
// 文件本体在 Files API 侧，块只是指向它的指针。请求侧代表「把这个已上传文件
// 送进容器输入目录」，响应侧代表「模型在容器里产出了这个文件」。
type ContainerUploadRef struct {
	FileID string `json:"file_id,omitempty"`
}

// Citation 正文中一段文字的来源标注。
//
// Start/End 是本块 Text 内的 rune 下标（半开区间），零值表示上游没给范围。
// 用 rune 而非 byte：Anthropic 与 OpenAI 的索引口径都是字符数，按字节算会让
// 中文引用整体错位。CitedText 是被引用的原文片段；两者互为冗余但都要保留，
// 因为各协议只给其中一种，缺的那种在编码时按另一种反推（参照 new-api
// claude_messages/citations.go 与 oai_chat/citations.go 的双向互推）。
type Citation struct {
	URL       string `json:"url,omitempty"`
	Title     string `json:"title,omitempty"`
	CitedText string `json:"cited_text,omitempty"`
	Start     int    `json:"start,omitempty"`
	End       int    `json:"end,omitempty"`
	// EncryptedIndex Anthropic 托管搜索回传时用的不透明游标。跨协议无对应槽位，
	// 但同协议往返必须原样带回，否则上游拒绝续话。
	EncryptedIndex string `json:"encrypted_index,omitempty"`
	// WireType 来源协议自报的引用种类（Anthropic 的 char_location /
	// page_location / content_block_location / search_result_location /
	// web_search_result_location）。空 = 来源协议的标注只有一种形态
	//（Chat/Responses 的 url_citation）。
	WireType string `json:"wire_type,omitempty"`
	// Raw 引用的原始块体。同族往返一律原样带回：官方 union 五种形态的字段
	// 互不相同（文档类靠 document_index 与页号/块下标/file_id 定位，托管搜索
	// 靠 url + encrypted_index），逐字段重建必造出上游不认的形状。
	// 属会话内容，不进日志与诊断注记。
	Raw json.RawMessage `json:"raw,omitempty"`
}

// HasRange 报告该引用是否带可用的正文范围。
func (c Citation) HasRange() bool { return c.End > c.Start }

// Portable 报告该引用能否落到外族协议的标注槽位上。Chat/Responses 的
// url_citation 都以 URL 作为来源身份；Anthropic 的文档类引用只有
// document_index 与页/块/字符下标，没有 URL，外族无从表达，只能干净丢弃
// 并报损耗（同族往返走 Raw，不受影响）。
func (c Citation) Portable() bool { return c.URL != "" }

// ServerToolUse 服务端托管工具调用（如上游代执行的 web_search）。
// 外形同 ToolUse，但结果由上游自己给出（BlockWebSearchToolResult），
// 客户端从不回结果。Input 与 ToolUse.Input 同规矩：流式期间逐片累积，
// 只有 BlockStop 之后才保证可解析。
type ServerToolUse struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Input string `json:"input,omitempty"`
}

// WebSearchToolResult web_search 托管工具的结果块。content 在上游是 union：
// 结果数组 或 错误对象（web_search_tool_result_error）。用 ErrorCode 判别——
// 缺这一维会把「搜索失败」按空结果数组解成「搜索成功但没找到东西」，
// 客户端基于假成功继续规划下一步。ErrorCode 非空时 Results 必为空。
type WebSearchToolResult struct {
	ToolUseID string            `json:"tool_use_id"`
	Results   []WebSearchResult `json:"results,omitempty"`
	ErrorCode string            `json:"error_code,omitempty"`
}

// WebSearchResult 单条搜索结果。Snippet 对应上游的 encrypted_content 字段
// （原文摘要，非加密，原样透传）。
type WebSearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
	PageAge string `json:"page_age,omitempty"`
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
	// FileID 是上游文件服务里的引用（Responses 的 input_image / input_file
	// 都收这一种载体）。本服务不代取文件内容，只在同族往返时原样带回；
	// 投给不认它的目标协议会丢，由有损诊断报告。
	FileID string `json:"file_id,omitempty"`
}

// HasPayload 是否有可投递的媒体载荷。
//
// Data 与 URL 全空的媒体块编不成任何协议的合法部件：anthropic 会写出一个
// 缺 media_type 与 data 的 base64 source，OpenAI 两系写出 url:"" 或连
// image_url 键都没有——都是上游按必填字段校验直接 400 的形状，而报错只说
// 媒体无效，读者看不出是哪一段输入害的。常见来源是 Responses 的 input_image
// 只给了 file_id，或客户端用了本层没建模的键名。
//
// FileID 刻意不算载荷：它只对 Responses 一族可投递，判「跨协议是否还有
// 东西可发」时必须排除。
func (m *Media) HasPayload() bool {
	return m != nil && (m.Data != "" || m.URL != "")
}

type ToolUse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Kind 是调用形态。零值 ToolFunction 表示 Input 承载 JSON 对象参数；
	// ToolCustom（responses 的 custom_tool_call）表示这是自由文本调用，
	// 原文在 InputText，Input 里放的是 {"input":<原文>} 投影——供没有
	// 自由文本槽位的协议（anthropic/chat/gemini）安全降级用。
	Kind ToolKind `json:"kind,omitempty"`
	// Input 是工具入参。function 形态下是 JSON 对象文本，流式解码期间是
	// 逐片累积的不完整 JSON，只有 BlockStop 之后才保证可解析；custom 形态
	// 下是 {"input":...} 投影（自由文本原文见 InputText）。
	Input string `json:"input,omitempty"`
	// InputText 是 custom 工具调用的自由文本原文。仅 Kind==ToolCustom 有意义：
	// responses 的 custom_tool_call.input 是一段自由文本而不是 JSON 参数，
	// 同族回写与流式增量都要用它，不能用投影（投影是给外族降级用的）。
	InputText string `json:"input_text,omitempty"`
	// Signature 是这次调用附带的推理签名。gemini 把它挂在 functionCall part
	// 自身而不是 thought part 上，所以它必须跟着调用走而不是跟着思考块走。
	//
	// 与 Thinking.Signature 同规矩：只在同族协议间透传，异族一律剥离并出说明。
	// 独立的一位而不是复用思考块那一位：一条响应里可以既有思考块的签名又有
	// 若干次调用各自的签名，它们分别对应上游的不同状态，混在一处就对不回去。
	Signature     string `json:"signature,omitempty"`
	SignatureFrom string `json:"signature_from,omitempty"`
	// ItemID 是 Responses function_call/custom_tool_call 条目的 item id
	// （fc_…/ctc_…），与 ID（call_id）是两个槽位。同族往返原样带回：
	// store=true 时上游存的条目按它索引，换成合成 id 后 item_reference
	// 全部错指。外族没有这一维，跨族丢弃由有损诊断报出。
	ItemID string `json:"item_id,omitempty"`
}

// ToolKind 工具调用形态。零值等同 function，保持既有构造与黄金文件兼容。
type ToolKind string

const (
	// ToolFunction 普通函数工具：入参是 JSON 对象。
	ToolFunction ToolKind = ""
	// ToolCustom 自定义工具（responses 的 custom_tool_call）：入参是自由文本。
	ToolCustom ToolKind = "custom"
)

// ObjectInput 返回对象槽位协议可承载的入参文本。function 形态原样返回
// Input（本就是 JSON 对象）；custom 形态返回 {"input":<原文>} 投影——
// anthropic 的 input、chat/responses 的 arguments、gemini 的 args 都只接
// JSON 对象，自由文本必须以投影形式落进去，既保住内容又不违反对象约束。
// 投影始终是合法对象，不会被 NormalizeToolInput 判成畸形。
func (t *ToolUse) ObjectInput() string {
	if t == nil {
		return "{}"
	}
	if t.Kind == ToolCustom {
		return string(MarshalCustomInput(t.InputText))
	}
	return t.Input
}

// MarshalCustomInput 把自由文本包成 {"input":<文本>} 投影。导出给 responses
// 的解码侧：请求历史里的 custom_tool_call 条目解码时就地把投影填好，
// 跨族编码器便可以只读 Input 而无需知道 Kind。
func MarshalCustomInput(text string) []byte {
	b, _ := json.Marshal(struct {
		Input string `json:"input"`
	}{Input: text})
	return b
}

type ToolResult struct {
	ToolUseID string `json:"tool_use_id"`
	// Kind 与产生它的调用同形态：custom 工具的结果回写 responses 时要落成
	// custom_tool_call_output 条目（而不是 function_call_output），否则同族
	// 往返会把自由文本调用的结果配到错误的条目类型上。
	Kind    ToolKind `json:"kind,omitempty"`
	Content []Block  `json:"content,omitempty"`
	IsError bool     `json:"is_error,omitempty"`
}

// Thinking 是推理内容。SignatureFrom 记录签名的来源协议，
// 因为签名只在同族协议间可透传：跨族（如 anthropic 的 signature 发给 responses）
// 上游会拒绝，必须丢弃。
type Thinking struct {
	Text          string `json:"text,omitempty"`
	Signature     string `json:"signature,omitempty"`
	SignatureFrom string `json:"signature_from,omitempty"`
	// Redacted 标记载荷是不可解读的加密推理（Anthropic 的 redacted_thinking）。
	// 这类块没有签名可言、且必须逐字回传：Anthropic 的续话校验要求上一轮的涂抹块
	// 原样带回，丢掉它会让安全系统涂抹过一次之后多轮对话直接断链。
	Redacted bool `json:"redacted,omitempty"`
	// RedactedData 是涂抹块的加密载荷原文（Anthropic redacted_thinking 的 data）。
	// 只在同族（anthropic↔anthropic）往返时有意义：出站编码逐字回吐，不解也不改
	// （上游是密文有效性的权威，本地判定只会误杀）。跨族协议没有对应槽位，一律
	// 丢弃并报有损——塞进别家的密文槽（Responses 的 encrypted_content、Gemini 的
	// thoughtSignature）等于伪造凭据，客户端下一轮回传必被拒。
	// 属会话内容，不进日志也不进诊断注记。
	RedactedData string `json:"redacted_data,omitempty"`
	// ItemID 是 Responses reasoning 条目的 item id（rs_…），同族往返原样
	// 带回，理由同 ToolUse.ItemID。
	ItemID string `json:"item_id,omitempty"`
	// ContentChannel 标记这段推理正文来自 Responses reasoning item 的
	// content 通道（reasoning_text，模型内部推理原文）而非 summary 通道
	//（reasoning_summary_text，给用户看的摘要）。两条通道官方并存、语义不同。
	// 默认 false=summary（历史行为）。同族编码按此标记选回哪条通道，避免把
	// content 原文塌缩进 summary 后客户端拿到的推理形态变了味。
	ContentChannel bool `json:"content_channel,omitempty"`
}

type Message struct {
	Role    Role    `json:"role"`
	Content []Block `json:"content"`
	// AudioID 是 chat assistant 历史消息的音频引用（官方请求侧只接受
	// {audio:{id}} 形态，完整音频数据不在多轮上下文里重复回传）。挂在消息上
	// 而不是块上：它没有内容本体，只是指向上游音频存储的 id。外族协议没有
	// 引用槽位，跨族丢弃由有损诊断报出。
	AudioID string `json:"audio_id,omitempty"`
	// Name 是 chat messages[].name（消息级发送者身份，群聊/agent 编排里
	// 区分同名角色的不同实体）。只有 chat 族有槽位：同族往返原样带回，
	// 跨族投影无处安放（与 user 维度的处置不同——那是会话级身份，
	// 这是消息级身份）。
	Name string `json:"name,omitempty"`
	// ItemID 是 Responses message 条目的 item id（msg_…），同族往返原样
	// 带回，理由同 ToolUse.ItemID。
	ItemID string `json:"item_id,omitempty"`
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
	// ServerRaw 服务端工具定义的原始 JSON（仅 anthropic 入站填充）。
	// 未建模的声明参数（computer 的 display_width_px/display_height_px、
	// web_fetch 的 citations/max_content_tokens 及未来新增键）逐字段建模
	// 跟进永远慢半拍，同族回写时整块原样吐出才能全保真。外族出站整条
	// 剔除该工具（dropServerTools）且线体结构没有原文槽位，天然到不了。
	// 字节内容视为不可变，Clone 随结构体值拷贝；omitempty 承重——IR 级
	// JSON 序列化下空值不得变成字面量 null 再被当成原文。
	ServerRaw json.RawMessage `json:"server_raw,omitempty"`
	// ServerParams 服务端工具的声明参数；nil 表示客户端一个参数都没给，
	// 同族回写时一个键也不造（缺省保持缺省）。同族往返优先走 ServerRaw
	// 整块原文，本槽位承担的是 IR 层的结构化可见性与无原文工具的可编码性。
	ServerParams *ServerParams `json:"server_params,omitempty"`
	// Strict 工具入参 schema 严格校验开关（anthropic tool.strict、OpenAI 两系
	// function.strict，同义同形）。三态指针：nil=没给（上游默认），显式
	// false 是「明确不要严格校验」，与没给语义不同。gemini 的工具定义
	// 没有这一维。
	Strict *bool `json:"strict,omitempty"`
	// 以下四维是 anthropic 工具定义的 2026 修饰槽位，其余协议的工具定义
	// 一个都没有（跨族丢+报）：
	// DeferLoading 工具不进初始 system prompt，由 tool search 按需加载。
	DeferLoading bool `json:"defer_loading,omitempty"`
	// EagerInputStreaming 细粒度流式入参（null=按 beta 头默认，三态指针）。
	EagerInputStreaming *bool `json:"eager_input_streaming,omitempty"`
	// InputExamples 入参示例（不透明对象数组，原文透传）。
	InputExamples []json.RawMessage `json:"input_examples,omitempty"`
	// AllowedCallers 允许的程序化调用方（direct / code_execution_*）。
	AllowedCallers []string `json:"allowed_callers,omitempty"`
	// CacheCtl/CacheTTL 工具定义上的缓存断点（anthropic
	// tools[].cache_control，含 ttl 档位）。其余协议的工具定义没有这一维，
	// 跨族丢弃由诊断报出（报数不报值）。
	CacheCtl string `json:"cache_ctl,omitempty"`
	CacheTTL string `json:"cache_ttl,omitempty"`
}

// ServerParams 服务端托管工具（web_search 一族）的声明参数。同族往返以
// Tool.ServerRaw 整块原文优先；本结构是参数的结构化视图——观测面（落库/
// 诊断）读它，内部构造的服务端工具（没有原文）靠它把参数编上线。
type ServerParams struct {
	// MaxUses 本回合最多调用次数（anthropic max_uses）。responses 无槽位。
	MaxUses int `json:"max_uses,omitempty"`
	// AllowedDomains 域名白名单（anthropic allowed_domains ⇔ responses
	// filters.allowed_domains，两族语义相同）。
	AllowedDomains []string `json:"allowed_domains,omitempty"`
	// BlockedDomains 域名黑名单（anthropic blocked_domains，与白名单互斥）。
	// responses 无槽位。
	BlockedDomains []string `json:"blocked_domains,omitempty"`
	// UserLocation 粗粒度地理位置（anthropic 与 responses 同为
	// {"type":"approximate",...} 形状，原文透传）。omitempty 承重：
	// RawMessage 不得伪造字面量 null。
	UserLocation json.RawMessage `json:"user_location,omitempty"`
	// SearchContextSize 检索规模档位 low/medium/high（responses 原生
	// search_context_size）。anthropic 无槽位；本仓 responses 入站对内建
	// 工具声明是跳过+注记，该字段暂无生产者——留着对齐参数全集，
	// responses 同族对称落地时启用。
	SearchContextSize string `json:"search_context_size,omitempty"`
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
	// AllowedTools 允许被调用的工具名白名单（responses/codex 的
	// tool_choice.type="allowed_tools"、gemini 的
	// functionCallingConfig.allowedFunctionNames）。与 Mode 正交：Mode 说
	// 要不要必须调，白名单说能调哪些。
	//
	// 四个出站都没有这一维的槽位——anthropic 官方 tool_choice 只有
	// auto/any/tool/none 四个变体，OpenAI 两系只能指名一个工具——但它可以
	// 被等价实现：把声明的工具列表收窄成白名单与已声明工具的交集，上游看
	// 不见别的工具就调不到。收窄见 AllowlistNarrow，落地在 codec.ShapeRequest。
	// 因此这一维通常不产生损耗，只有白名单与已声明工具全无交集时才无从
	// 收窄，由整形阶段报出。
	AllowedTools []string `json:"allowed_tools,omitempty"`
	// Raw responses 一族 typed tool_choice 变体的不透明槽：{"type":"mcp"}、
	// {"type":"file_search"} 等托管工具指名形态（官方 ToolChoiceTypesParam /
	// ToolChoiceMcpParam），字段互不相同且随官方演进，没有跨族统一维度可
	// 建模。Mode 留零值：外族出站编不出对应形状（tool_choice 缺省），损耗
	// 由 DescribeLossy 报出；同族出站原样回写，一个字节不变。字节内容视为
	// 不可变，Clone 随结构体值拷贝；omitempty 承重——IR 级 JSON 序列化下
	// 空值不得变成字面量 null 再被当成原文。
	Raw json.RawMessage `json:"raw,omitempty"`
}

// AllowlistApplies 白名单是否落在「靠收窄实现」的模式上。
//
// 指名调用（ToolChoiceTool）上游本来就只会调那一个，禁止调用
// （ToolChoiceNone）一个都不调，两者都不需要收窄。把它们算进来会让这两
// 种模式恒报「限制失效」。
func (tc *ToolChoice) AllowlistApplies() bool {
	if tc == nil || len(tc.AllowedTools) == 0 {
		return false
	}
	return tc.Mode == ToolChoiceAuto || tc.Mode == ToolChoiceAny
}

// AllowlistNarrow 按工具白名单收窄已声明的工具，返回收窄后的列表与「限制
// 是否真的落得下去」。纯函数：整形阶段用它改写请求，测试用它断言判据，
// 收窄与报错必须出自同一个函数才不会漂移——漂移之后要么明明收窄成功却
// 照报损耗，要么明明无从收窄却不报（模型照样能调被禁的工具）。
//
// 落不下去（第二个返回值为 false）只有一种情形：白名单里的名字一个都不在
// 已声明的非服务端工具里。此时原样返回整个列表——收窄到零个工具会连锁
// 触发「零工具丢 tool_choice」乃至工具历史降级，那是比白名单失效大得多
// 的破坏。白名单里写了未声明的名字本身不算损耗：那个名字压根不存在，
// 模型调不到它，客户端要的限制照样成立。
//
// 服务端工具（ServerType 非空）一律保留且不计入交集：白名单管的是客户
// 端声明的函数，把托管搜索一类连带删掉是删了客户端声明过的东西，比白
// 名单失效更糟。
func (r *Request) AllowlistNarrow() ([]Tool, bool) {
	if r == nil || !r.ToolChoice.AllowlistApplies() {
		return nil, false
	}
	allowed := make(map[string]bool, len(r.ToolChoice.AllowedTools))
	for _, name := range r.ToolChoice.AllowedTools {
		allowed[name] = true
	}
	kept := make([]Tool, 0, len(r.Tools))
	matched := 0
	for _, t := range r.Tools {
		if t.ServerType != "" {
			kept = append(kept, t)
			continue
		}
		if allowed[t.Name] {
			kept = append(kept, t)
			matched++
		}
	}
	if matched == 0 {
		return r.Tools, false
	}
	return kept, true
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

	// Adaptive 模型自主决定思考量（Anthropic thinking.type=adaptive，官方
	// 已标 enabled 废弃）。与 BudgetTokens 互斥：adaptive 不带预算。其余
	// 协议没有「自适应」这一档——OpenAI 的 effort 是显式档位，跨族时降级
	// 成固定档并由诊断报出。
	Adaptive bool `json:"adaptive,omitempty"`
	// Display 思考内容回显形态（Anthropic thinking.display：
	// "summarized"=正常回显 / "omitted"=只回签名供多轮续接）。仅 Anthropic
	// 有这一维，跨族丢弃并报出。
	Display string `json:"display,omitempty"`

	// Summary 思考摘要的啰嗦程度（OpenAI Responses reasoning.summary：
	// auto/concise/detailed）。与 Display 不同轴：Display 管可见性，Summary
	// 管摘要详略，两者不构成等价物。仅 responses 一族有槽位，跨族丢弃并报出。
	Summary string `json:"summary,omitempty"`
	// Context / Mode reasoning 的另两维（context: auto/current_turn/all_turns；
	// mode: standard/pro）。值形态仍在演进，按原文收下不解析（与 Moderation /
	// Prediction 同款约定），仅 responses 一族能回写。
	// omitempty 必须带：这两个键会随 ir.Request 走 JSON 序列化（流水脱敏、
	// 重放），nil 不带标签会变成非空 "null"，出站据此判断「客户端给过」
	// 就会凭空写出一个 context:null。
	Context json.RawMessage `json:"context,omitempty"`
	Mode    json.RawMessage `json:"mode,omitempty"`
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

	MaxTokens int `json:"max_tokens,omitempty"`
	// MaxCompletionKey 客户端用的是现代键名 max_completion_tokens（true）
	// 还是已废弃的 max_tokens（false）。官方注明旧键不兼容 o 系推理模型，
	// chat 同族往返时原键名带回；跨族投影不受影响（别的协议没有这对键名）。
	MaxCompletionKey bool     `json:"max_completion_key,omitempty"`
	Temperature      *float64 `json:"temperature,omitempty"`
	TopP             *float64 `json:"top_p,omitempty"`
	TopK             *int     `json:"top_k,omitempty"`
	StopSequences    []string `json:"stop_sequences,omitempty"`
	Stream           bool     `json:"stream,omitempty"`

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
	// ServiceTier 是计费与优先级档位。保留原值不规整（anthropic
	// auto/standard_only；OpenAI 两系 auto/default/flex/scale/priority/
	// fast，responses 另有 ultrafast）：跨族映射在出站编码按目标协议
	// 值集进行（codec.MapServiceTier），装不下的档位丢弃并由诊断报出。
	ServiceTier string `json:"service_tier,omitempty"`
	// PromptCacheKey 提示缓存路由键（OpenAI 两系的 prompt_cache_key）。
	// 值可能是客户端自选串，诊断与日志一律不回显值本身。
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
	// Moderation 请求级审核策略（OpenAI 两系 {model, policy{input/output}}）。
	// 不透明原文透传：代理不解释审核策略，只负责送达或报出。字节按
	// RawMessage 不可变惯例随值共享（Clone 同 Prediction 的处理）。
	Moderation json.RawMessage `json:"moderation,omitempty"`
	// PromptCacheOptions 显式缓存断点控制（OpenAI 两系 {mode, ttl, ...}）。
	// 不透明原文透传，共享惯例同 Moderation。
	PromptCacheOptions json.RawMessage `json:"prompt_cache_options,omitempty"`

	// 以下两维只有 Anthropic 一族有，外族没有任何对应物。收进 IR 只为
	// 同协议回写 + 跨协议诊断，不作映射尝试。
	// TopCacheCtl 顶层 cache_control 便捷糖的 type（如 "ephemeral"）。
	// 官方语义是「自动给最后一个可缓存块打缓存断点」；IR 不展开成块级——
	// 展开要猜「最后一个可缓存块」是哪一个（tools→system→messages 的查找
	// 顺序官方没钉死），猜错位置比不展开更糟。原样保留顶层形态。
	TopCacheCtl string `json:"top_cache_ctl,omitempty"`
	// TopCacheTTL 顶层糖的存活档位（"5m"/"1h"，空=官方默认 5m）。
	TopCacheTTL string `json:"top_cache_ttl,omitempty"`
	// InferenceGeo 推理地理偏好（inference_geo，如 "us"）。空=没给，
	// 上游按 workspace 的 default_inference_geo 处理；显式 null 与缺省
	// 在 JSON 层同义，解码后都是空。诊断与日志一律不回显值本身。
	InferenceGeo string `json:"inference_geo,omitempty"`
	// Container 代码执行容器复用标识与技能声明（anthropic 的 container
	// 参数，string 简写与 {id,skills} 对象两形态统一成此结构；外族无对应，
	// 跨族由诊断报出）。nil = 客户端没提。
	Container *Container `json:"container,omitempty"`
	// ParallelToolCalls 是否允许一轮里并行多个工具调用。三态指针：
	// 没给就不替客户端表态（同 ThinkingConfig.Enabled 的判据）。
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`
	// ResponseFormat 是结构化输出要求。
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
	// Verbosity 是输出详略档位（low/medium/high）。分族落位：chat 是顶层
	// verbosity，responses 是 text.verbosity；其余两族没有输出长度转向
	// 这一维，跨族丢弃由诊断报出。
	Verbosity string `json:"verbosity,omitempty"`
	// SafetyIdentifier 滥用检测标识（OpenAI 两系的 safety_identifier，
	// user 字段的官方替代）。与 Metadata 的 user_id 同一维度：跨族到
	// anthropic 时映进 metadata.user_id 槽位（该槽被 user_id 占了才丢，
	// 丢要报）。值是用户标识，诊断与日志不回显。
	SafetyIdentifier string `json:"safety_identifier,omitempty"`
	// Include 要求上游额外返回哪些内容（responses 的 include）。
	Include []string `json:"include,omitempty"`
	// Background 是后台运行模式（responses 的 background）。三态指针：
	// nil = 客户端没提；显式 false 等同默认，都不算表态。显式 true 时
	// 客户端期待的是「立刻拿任务 id、稍后取回」的异步行为；本服务是同步
	// 流式中继（对上游一律 stream + store:false），兑现不了任何协议的
	// background，出站一律不回写并由诊断报出。收进 IR 只为可见与可报。
	Background *bool `json:"background,omitempty"`
	// Truncation 是上游侧的历史截断策略（responses 的 truncation）。
	Truncation string `json:"truncation,omitempty"`
	// MaxToolCalls 单轮响应允许的工具调用总上限（responses 一族的
	// max_tool_calls）。三态指针：nil = 客户端没提。其余三族没有计数
	// 闸门，客户端要的安全上限跨族不再生效，由诊断报出。
	MaxToolCalls *int `json:"max_tool_calls,omitempty"`
	// IncludeObfuscation 流式混淆开关（OpenAI 两系的 stream_options.
	// include_obfuscation）。三态指针：显式 false 是「关掉上游默认开着的
	// 混淆保护」，与没提不是一回事，两态布尔会把显式 false 吞回缺省。
	// anthropic 与 gemini 的流式帧没有混淆机制，跨族由诊断报出。
	IncludeObfuscation *bool `json:"include_obfuscation,omitempty"`
	// ClientMetadata 是客户端自定义元数据。与 Metadata 分开：后者只承载
	// user_id 且被翻译成各协议的用户标识字段，混在一起会让 user_id
	// 既作为用户标识、又作为一条普通元数据发出去两次。
	ClientMetadata map[string]string `json:"client_metadata,omitempty"`
	// IncludeUsage 是客户端对「要不要那一帧单独的 usage」的表态。
	//
	// 与「向上游要不要 usage」是两件事：对上游一律要（记账要用），
	// 这一维只管转不转给客户端。三态指针而不是 bool：没给与明确 true
	// 在当前行为上相同（都发），但合并后就没有位置表达「客户端明确要」，
	// 将来若要把默认改成不发，那两种必须分开。
	//
	// 只有 Chat Completions 有这个开关。Anthropic 的 message_delta 带 usage
	// 与 Responses 的 response.completed 带 usage 都是协议固有形状，
	// 不是可选帧。
	IncludeUsage *bool `json:"include_usage,omitempty"`

	// 以下四维只有 chat_completions 一族有（responses 全系无对应槽位）。
	// 收进 IR 只为同族往返与跨族损耗诊断，不作映射尝试。
	// Modalities 输出模态（"text"/"audio"）。
	Modalities []string `json:"modalities,omitempty"`
	// AudioOut 音频输出配置。仅 Modalities 含 "audio" 时有效，
	// 同给与否都透传让上游判定。
	AudioOut *AudioOut `json:"audio_out,omitempty"`
	// Prediction 预测输出配置（重生成场景提速，{type:"content",content:...}）。
	// 内容嵌套，不透明原文透传；显式 null 归一为没给。omitempty 承重。
	Prediction json.RawMessage `json:"prediction,omitempty"`
	// WebSearchOptions 联网搜索选项（{search_context_size,user_location}）。
	// 不透明原文透传；显式 null 归一为没给。omitempty 承重。
	WebSearchOptions json.RawMessage `json:"web_search_options,omitempty"`
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
	// Description schema 的自然语言说明（OpenAI 两系的 json_schema.
	// description，chat 嵌套在 response_format.json_schema 下、responses
	// 平铺在 text.format 下，同键同义）。anthropic 的 output_config.format
	// 与 gemini 的 responseSchema 都没有这一键，跨族丢弃由诊断报出。
	Description string `json:"description,omitempty"`
	// Strict 要求上游严格遵循 schema。三态指针：各家默认值不同，
	// 客户端没表态时不替它选。
	Strict *bool `json:"strict,omitempty"`
}

// AudioOut 是 chat 音频输出配置。Format 取 wav/aac/mp3/flac/opus/pcm16；
// Voice 是内置音色名或自定义音色 id——官方给两形态（string 或 {id} 对象），
// 解码时归一成 string（语义等价），回写恒写 string 简形；没给 voice 时
// 保持空，不造键（空串会被上游当成非法音色名）。
type AudioOut struct {
	Format string `json:"format,omitempty"`
	Voice  string `json:"voice,omitempty"`
}

// AudioOutput 是 chat 非流式响应的模型音频输出（message.audio）。
// ID 是下一轮 assistant 历史唯一允许回传的引用（进 Message.AudioID）；
// Data/ExpiresAt/Transcript 只属于本轮完整响应，不能塞回请求——官方
// 请求侧的 audio 只接受 {id} 形态。
//
// 完整音频只存在于 chat 非流式这一处：chat 的 SSE delta 没有官方音频槽位，
// anthropic 与 responses 的响应也没有等价物，编码边界必须丢弃并报出。
type AudioOutput struct {
	ID         string `json:"id,omitempty"`
	Data       string `json:"data,omitempty"`
	ExpiresAt  int64  `json:"expires_at,omitempty"`
	Transcript string `json:"transcript,omitempty"`
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
	if r.Tools != nil {
		out.Tools = make([]Tool, len(r.Tools))
		for i, tl := range r.Tools {
			tl.Strict = cloneBool(tl.Strict)
			tl.EagerInputStreaming = cloneBool(tl.EagerInputStreaming)
			tl.InputExamples = append([]json.RawMessage(nil), tl.InputExamples...)
			tl.AllowedCallers = append([]string(nil), tl.AllowedCallers...)
			if tl.ServerParams != nil {
				// ServerRaw 字节视为不可变随值共享；ServerParams 是指针，
				// 必须换头，切片容器照 InputExamples 的惯例各自复制。
				sp := *tl.ServerParams
				sp.AllowedDomains = append([]string(nil), sp.AllowedDomains...)
				sp.BlockedDomains = append([]string(nil), sp.BlockedDomains...)
				tl.ServerParams = &sp
			}
			out.Tools[i] = tl
		}
	}
	out.StopSequences = append([]string(nil), r.StopSequences...)
	out.DecodeNotes = append([]string(nil), r.DecodeNotes...)
	out.Modalities = append([]string(nil), r.Modalities...)
	// Prediction/WebSearchOptions 字节按 RawMessage 不可变惯例随值共享。
	if r.AudioOut != nil {
		ao := *r.AudioOut
		out.AudioOut = &ao
	}
	if r.ToolChoice != nil {
		tc := *r.ToolChoice
		tc.AllowedTools = append([]string(nil), r.ToolChoice.AllowedTools...)
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
	out.Background = cloneBool(r.Background)
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
	out.IncludeUsage = cloneBool(r.IncludeUsage)
	out.MaxToolCalls = cloneInt(r.MaxToolCalls)
	out.IncludeObfuscation = cloneBool(r.IncludeObfuscation)
	if r.ResponseFormat != nil {
		rf := *r.ResponseFormat
		rf.Strict = cloneBool(r.ResponseFormat.Strict)
		out.ResponseFormat = &rf
	}
	if r.Container != nil {
		ct := *r.Container
		if r.Container.Skills != nil {
			ct.Skills = append([]Skill(nil), r.Container.Skills...)
		}
		out.Container = &ct
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
		out[i] = Message{Role: m.Role, Content: cloneBlocks(m.Content),
			AudioID: m.AudioID, Name: m.Name, ItemID: m.ItemID}
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
		if b.ServerToolUse != nil {
			v := *b.ServerToolUse
			out[i].ServerToolUse = &v
		}
		if b.WebSearchToolResult != nil {
			v := *b.WebSearchToolResult
			if b.WebSearchToolResult.Results != nil {
				v.Results = append([]WebSearchResult(nil), b.WebSearchToolResult.Results...)
			}
			out[i].WebSearchToolResult = &v
		}
		if b.ContainerUpload != nil {
			v := *b.ContainerUpload
			out[i].ContainerUpload = &v
		}
		if b.Citations != nil {
			out[i].Citations = append([]Citation(nil), b.Citations...)
		}
	}
	return out
}
