package ir

type StopReason string

const (
	StopEndTurn       StopReason = "end_turn"
	StopMaxTokens     StopReason = "max_tokens"
	StopStopSequence  StopReason = "stop_sequence"
	StopToolUse       StopReason = "tool_use"
	StopContentFilter StopReason = "content_filter"
)

type Usage struct {
	InputTokens      int64 `json:"input_tokens,omitempty"`
	OutputTokens     int64 `json:"output_tokens,omitempty"`
	CacheReadTokens  int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
}

// MergeUsage 把一帧 usage 并入累加器，逐字段取较大值。
//
// 用量分散在多帧：Anthropic 在 message_start 给 input、message_delta 给 output；
// Chat Completions 只在末尾单独发一帧。取较大值而非累加，是因为部分上游
// 每帧都重发累计值，累加会翻倍。
func MergeUsage(into *Usage, u Usage) {
	if u.InputTokens > into.InputTokens {
		into.InputTokens = u.InputTokens
	}
	if u.OutputTokens > into.OutputTokens {
		into.OutputTokens = u.OutputTokens
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
