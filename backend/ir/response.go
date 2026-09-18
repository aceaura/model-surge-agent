package ir

type StopReason string

const (
	StopEndTurn       StopReason = "end_turn"
	StopMaxTokens     StopReason = "max_tokens"
	StopStopSequence  StopReason = "stop_sequence"
	StopToolUse       StopReason = "tool_use"
	StopContentFilter StopReason = "content_filter"
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
	// ReasoningTokens 是推理消耗，计费上属于输出，故已计入 OutputTokens。
	// 独立承载只为成本归因：推理占比看不见时，一个模型贵在哪儿无从判断。
	//
	// 不对它与 OutputTokens 做「不得更大」的钳制：部分上游把两者作为
	// 独立计量而非包含关系给出，钳制会把上游的真实数字改掉。
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
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
	if u.CacheReadTokens > into.CacheReadTokens {
		into.CacheReadTokens = u.CacheReadTokens
	}
	if u.CacheWriteTokens > into.CacheWriteTokens {
		into.CacheWriteTokens = u.CacheWriteTokens
	}
}

type Response struct {
	ID         string     `json:"id,omitempty"`
	Model      string     `json:"model,omitempty"`
	Content    []Block    `json:"content"`
	StopReason StopReason `json:"stop_reason,omitempty"`
	Usage      Usage      `json:"usage"`
}
