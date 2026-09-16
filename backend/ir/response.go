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

type Response struct {
	ID         string     `json:"id,omitempty"`
	Model      string     `json:"model,omitempty"`
	Content    []Block    `json:"content"`
	StopReason StopReason `json:"stop_reason,omitempty"`
	Usage      Usage      `json:"usage"`
}
