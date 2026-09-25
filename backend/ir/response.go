package ir

type StopReason string

const (
	StopEndTurn       StopReason = "end_turn"
	StopMaxTokens     StopReason = "max_tokens"
	StopStopSequence  StopReason = "stop_sequence"
	StopToolUse       StopReason = "tool_use"
	StopContentFilter StopReason = "content_filter"
	// StopContextWindow 输入占满上下文窗口挤断输出（anthropic 官方 beta 档
	// model_context_window_exceeded）。单列而不并进 StopMaxTokens：两者的
	// 客户端补救动作相反——这个要压缩输入，max_tokens 要抬输出配额。
	// 外族协议没有对应取值，出站按各协议的「输出不完整」档投影。
	StopContextWindow StopReason = "context_window_exceeded"
	// StopMaxMessages 消息数上限截断（responses incomplete_details.reason
	// 的 "max_messages"）。与 max_tokens 的输出长度上限是两回事：客户端照
	// max_tokens 的提示加大输出预算重试，仍会被同一上限拦住。只有
	// responses 一族有此档，外族出站归「输出不完整」档。
	StopMaxMessages StopReason = "max_messages"
)

// Usage 是一次调用的 token 用量。
//
// InputTokens 的口径是**不含缓存命中的新鲜输入**：这是四个协议里唯一
// 无歧义的定义。Anthropic 原生就是这个口径；chat_completions 与 responses
// 的 prompt_tokens 含缓存，所以解码时要减去、编码回去时要加回。
// 客户端可见的输入总量恒等于 InputTokens + CacheReadTokens。
type Usage struct {
	InputTokens      int64 `json:"input_tokens,omitempty"`
	OutputTokens     int64 `json:"output_tokens,omitempty"`
	CacheReadTokens  int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
	// CacheWrite5mTokens / CacheWrite1hTokens 是缓存写入按 TTL 档的明细
	//（Anthropic 的 cache_creation.ephemeral_5m/1h_input_tokens），两档
	// 单价不同（1h 写入通常 2×、5m 1.25×），只有总量时成本归因做不了。
	// 仅 anthropic 上游给得出；跨族投影时明细丢弃、总量保留，丢弃由诊断报出。
	CacheWrite5mTokens int64 `json:"cache_write_5m_tokens,omitempty"`
	CacheWrite1hTokens int64 `json:"cache_write_1h_tokens,omitempty"`
	// CacheWriteDetailsKnown 标记上面两位明细可信（含「明细确实是零」）。
	// 没有它，全零的明细与「上游没给明细」无法区分：前者出站要照实写出
	// cache_creation 对象，后者写了等于伪造精度。
	CacheWriteDetailsKnown bool `json:"cache_write_details_known,omitempty"`
	// ReasoningTokens 是推理消耗，计费上属于输出，故已计入 OutputTokens。
	// 独立承载只为成本归因：推理占比看不见时，一个模型贵在哪儿无从判断。
	//
	// 不对它与 OutputTokens 做「不得更大」的钳制：部分上游把两者作为
	// 独立计量而非包含关系给出，钳制会把上游的真实数字改掉。
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
	// WebSearchRequests / WebFetchRequests 服务端托管工具的执行次数
	//（Anthropic 的 usage.server_tool_use）。是次数不是 token，不进任何
	// 合计；按次计费，看不见就无法对账托管搜索的成本。
	WebSearchRequests int64 `json:"web_search_requests,omitempty"`
	WebFetchRequests  int64 `json:"web_fetch_requests,omitempty"`
	// PromptAudioTokens / CompletionAudioTokens Chat 音频 token 明细
	//（prompt_tokens_details / completion_tokens_details 的 audio_tokens）。
	// 各自是所在总量的子集而非另一项，不参与合计。
	PromptAudioTokens     int64 `json:"prompt_audio_tokens,omitempty"`
	CompletionAudioTokens int64 `json:"completion_audio_tokens,omitempty"`
	// AcceptedPredictionTokens / RejectedPredictionTokens Chat 预测加速
	//（completion_tokens_details 的 accepted/rejected_prediction_tokens），
	// 同为输出总量的子集。
	AcceptedPredictionTokens int64 `json:"accepted_prediction_tokens,omitempty"`
	RejectedPredictionTokens int64 `json:"rejected_prediction_tokens,omitempty"`
	// InferenceGeo Anthropic 响应侧回显的实际推理区域（usage.inference_geo）。
	// 请求侧的同名偏好字段在 Request 上；这里只是回执，不参与调度，
	// 也仅同族出站写得回去。
	InferenceGeo string `json:"inference_geo,omitempty"`
}

// MergeUsage 把一帧 usage 并入累加器。
//
// 用量分散在多帧：Anthropic 在 message_start 给 input、message_delta 给 output；
// Chat Completions 只在末尾单独发一帧。都不是累加语义——部分上游每帧重发
// 累计值，累加会翻倍。
//
// 输入与输出用「后到覆盖先到」：同一次流可能从两个位置给出 usage
// （事件顶层与 response 对象内），后到的那份口径更完整。取较大值会让
// 先到的偏大值粘住，减去缓存量后的修正值再也盖不回去。
// 缓存字段仍取较大值：它在流中通常只出现一次，取 max 能容忍缺帧。
func MergeUsage(into *Usage, u Usage) {
	if u.InputTokens > 0 {
		into.InputTokens = u.InputTokens
	}
	if u.OutputTokens > 0 {
		into.OutputTokens = u.OutputTokens
	}
	// 与输出同口径：末尾那帧的数字才是完整的，但缺了这一维的帧不该把它清零。
	if u.ReasoningTokens > 0 {
		into.ReasoningTokens = u.ReasoningTokens
	}
	// 托管工具次数与音频/预测明细同样只在收尾帧出现一次，非零后到覆盖。
	if u.WebSearchRequests > 0 {
		into.WebSearchRequests = u.WebSearchRequests
	}
	if u.WebFetchRequests > 0 {
		into.WebFetchRequests = u.WebFetchRequests
	}
	if u.PromptAudioTokens > 0 {
		into.PromptAudioTokens = u.PromptAudioTokens
	}
	if u.CompletionAudioTokens > 0 {
		into.CompletionAudioTokens = u.CompletionAudioTokens
	}
	if u.AcceptedPredictionTokens > 0 {
		into.AcceptedPredictionTokens = u.AcceptedPredictionTokens
	}
	if u.RejectedPredictionTokens > 0 {
		into.RejectedPredictionTokens = u.RejectedPredictionTokens
	}
	if u.InferenceGeo != "" {
		into.InferenceGeo = u.InferenceGeo
	}
	if u.CacheReadTokens > into.CacheReadTokens {
		into.CacheReadTokens = u.CacheReadTokens
	}
	if u.CacheWriteTokens > into.CacheWriteTokens {
		into.CacheWriteTokens = u.CacheWriteTokens
	}
	// TTL 明细整组随「已知」标记走：带明细的帧到达时两位一起覆盖，
	// 不带明细的帧不清零已知的明细（同输入输出的「缺帧不清零」口径）。
	if u.CacheWriteDetailsKnown {
		into.CacheWrite5mTokens = u.CacheWrite5mTokens
		into.CacheWrite1hTokens = u.CacheWrite1hTokens
		into.CacheWriteDetailsKnown = true
	}
}

// StopDetails 拒绝停止的结构化分类（仅 anthropic：message_delta 与非流式
// 响应的 stop_details，type 恒 "refusal"）。其余协议无槽位，跨族出站
// 不投影也不注记——分类文本官方注明不稳定，价值止于同族回显。
type StopDetails struct {
	// Category 触发拒绝的策略分类（cyber/bio 等）；上游显式 null 与缺省
	// 同归空串（官方注明二者语义相同）。
	Category string `json:"category,omitempty"`
	// Explanation 人类可读解释，官方注明文本不稳定。
	Explanation string `json:"explanation,omitempty"`
}

type Response struct {
	ID         string     `json:"id,omitempty"`
	Model      string     `json:"model,omitempty"`
	Content    []Block    `json:"content"`
	StopReason StopReason `json:"stop_reason,omitempty"`
	// StopSequence 是触发停止的那一条序列的原文。空表示不是由停止序列
	// 结束的，或上游没给。
	//
	// 只有 StopReason 为 StopStopSequence 时有意义：按它切分输出的客户端
	// 拿到一条未触发的序列会切错位置，比拿不到更坏。
	StopSequence string `json:"stop_sequence,omitempty"`
	// StopDetails 拒绝档的结构化分类；非拒绝或上游未给为 nil。
	StopDetails *StopDetails `json:"stop_details,omitempty"`
	Usage       Usage        `json:"usage"`
	// ServiceTier 是上游实际执行时所用的档位，原样回显。
	//
	// 绝不拿请求里的值兜底：客户端点了 flex 而上游降到 default 时，
	// 兜底会把「降档了」伪装成「按你要的档位执行了」，而这一维决定计费。
	ServiceTier string `json:"service_tier,omitempty"`
	// SystemFingerprint Chat 后端配置指纹回显（系统版本变化信号，排障用）。
	// 仅 chat 族有槽位，跨族出站不投影。
	SystemFingerprint string `json:"system_fingerprint,omitempty"`
	// Container 实际使用的代码执行容器回显（仅 anthropic：id/expires_at/
	// 已加载技能）。nil = 上游没用容器。客户端要靠它复用容器续话。
	Container *Container `json:"container,omitempty"`
	// Audio chat 非流式的模型音频输出。流式 chat delta 没有官方音频槽位，
	// 非 chat 客户端也无从接收，编码边界必须丢弃并报出。
	Audio *AudioOutput `json:"audio,omitempty"`
	// Created 上游回显的创建时间（chat created / responses created_at，Unix
	// 秒）。零值=上游没给，出站才回退本地钟——否则同族往返会把上游的真实
	// 创建时间换成代理本地钟，客户端按 created 做幂等/排序会拿到假数据。
	Created int64 `json:"created,omitempty"`
}
