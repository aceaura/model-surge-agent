package ir

// 事件词汇取 Anthropic streaming 为超集：四协议中只有它显式表达
// 块生命周期、增量类型与独立的 usage 帧。Chat Completions 的 choices[].delta
// 与 Gemini 的 candidates[].parts 都是「同一块隐式续写」，能无损投影进块模型；
// 反向（把块模型压成隐式续写）会丢掉块边界，所以只能以 Anthropic 为超集。
type EventType string

const (
	// EvMessageStart 一次响应的开始，携带 MessageID 与 Model。
	EvMessageStart EventType = "message_start"
	// EvBlockStart 开一个内容块，Index 是块序号，Block 给出块类型与初始内容。
	EvBlockStart EventType = "block_start"
	// EvTextDelta / EvThinkingDelta / EvSigDelta / EvToolInput 是块内增量，
	// 都以 Text 承载片段，靠 Type 区分落点。
	EvTextDelta     EventType = "text_delta"
	EvThinkingDelta EventType = "thinking_delta"
	EvSigDelta      EventType = "signature_delta"
	EvToolInput     EventType = "tool_input_delta"
	// EvCitation 引用标注增量，Citations 为本次新增的来源。
	// 命名取 Anthropic 的 citations_delta 帧：事件词汇以它为超集。
	EvCitation EventType = "citation_delta"
	// EvBlockStop 关闭 Index 指向的块。
	EvBlockStop EventType = "block_stop"
	// EvMessageDelta 携带 StopReason 与最终 Usage。
	EvMessageDelta EventType = "message_delta"
	EvMessageStop  EventType = "message_stop"
	// EvPing 是保活帧，无语义，可丢。
	EvPing EventType = "ping"
	// EvError 是流内错误。出现后流即终止。
	EvError EventType = "error"
)

type Event struct {
	Type  EventType `json:"type"`
	Index int       `json:"index,omitempty"`
	// Block 仅在 EvBlockStart 出现，描述块类型与初始内容。
	Block *Block `json:"block,omitempty"`
	// Text 承载各类 delta 的片段内容。
	Text string `json:"text,omitempty"`
	// SignatureFrom 只在 EvSigDelta 上有意义，记录签名的来源协议。
	//
	// 块上的 Thinking.SignatureFrom 不够用：流式编码器逐帧处理，块开始与
	// 签名增量之间可能隔任意多帧，编码器手里只有当前这一帧，没有块的全貌，
	// 判不出同族就会把异族签名照原样写给客户端。
	SignatureFrom string `json:"signature_from,omitempty"`
	// ServiceTier 可以出现在 EvMessageStart 或 EvMessageDelta 上。
	//
	// 两处都允许而不是钉死一处：chat_completions 把它放在每个 chunk 的
	// 顶层，responses 放在 response 对象里、随 created 与 completed 两次
	// 出现。哪一帧先到取决于上游，只认一处就会在另一种形态下丢。
	ServiceTier string `json:"service_tier,omitempty"`
	// Container 代码执行容器回显（EvMessageStart 首帧携带；anthropic 的
	// message_delta 也可能晚到，EvMessageDelta 上也收，后值覆盖）。
	Container  *Container `json:"container,omitempty"`
	StopReason StopReason `json:"stop_reason,omitempty"`
	// StopSequence 与 Response.StopSequence 同义，随收尾帧抵达。
	StopSequence string `json:"stop_sequence,omitempty"`
	Usage        *Usage `json:"usage,omitempty"`
	MessageID    string `json:"message_id,omitempty"`
	Model        string `json:"model,omitempty"`
	// Created 是上游回显的创建时间（chat created / responses created_at，
	// Unix 秒），只在 EvMessageStart 上有意义。零值=上游没给，出站才回退
	// 本地钟——否则同族往返会把上游的真实创建时间换成代理本地钟，
	// 客户端按 created 做幂等/排序会拿到假数据。
	Created int64  `json:"created,omitempty"`
	Err     *Error `json:"error,omitempty"`
	// Citations 仅在 EvCitation 出现，携带本次新增的来源标注。
	Citations []Citation `json:"citations,omitempty"`
}
