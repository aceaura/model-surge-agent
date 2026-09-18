// Package agentv1 是管理面的对外 DTO。
//
// 单独一包是为了让前端有一份权威的字段定义可镜像，也让内部类型
// （pipeline.Record、store.Entry）能自由改动而不动摇 API 契约。
package agentv1

import "time"

type ErrorEnvelope struct {
	Error Error `json:"error"`
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// 管理面错误码。前端按码分支，不解析消息文本。
const (
	CodeUnauthorized   = "unauthorized"
	CodeNotFound       = "not_found"
	CodeInvalidRequest = "invalid_request"
	CodeUnavailable    = "unavailable"
	CodeInternal       = "internal_error"
)

type Health struct {
	Status        string `json:"status"`
	Database      string `json:"database"`
	Cache         string `json:"cache"`
	Relay         string `json:"relay"`
	OutboxPending int    `json:"outbox_pending"`
	OutboxDead    int    `json:"outbox_dead"`
}

// RequestSummary 是一条请求流水。绝不含凭据与对话内容。
type RequestSummary struct {
	RequestID        string    `json:"request_id"`
	At               time.Time `json:"at"`
	InboundProtocol  string    `json:"inbound_protocol"`
	OutboundProtocol string    `json:"outbound_protocol,omitempty"`
	UserModel        string    `json:"user_model"`
	ModelID          string    `json:"model_id,omitempty"`
	Account          string    `json:"account,omitempty"`
	Outcome          string    `json:"outcome"`
	StatusCode       int       `json:"status_code,omitempty"`
	Attempts         int       `json:"attempts,omitempty"`
	TriedIDs         []string  `json:"tried_ids,omitempty"`
	Committed        bool      `json:"committed,omitempty"`
	Stream           bool      `json:"stream,omitempty"`
	UsageEstimated   bool      `json:"usage_estimated,omitempty"`
	InputTokens      int64     `json:"input_tokens,omitempty"`
	OutputTokens     int64     `json:"output_tokens,omitempty"`
	CacheReadTokens  int64     `json:"cache_read_tokens,omitempty"`
	LatencyMS        int       `json:"latency_ms,omitempty"`
	FirstTokenMS     int       `json:"first_token_ms,omitempty"`
	ErrorCode        string    `json:"error_code,omitempty"`
	ErrorMessage     string    `json:"error_message,omitempty"`
	// Sanitized 是对客户端请求所做的畸形修复说明；为空表示请求本身合法。
	Sanitized []string `json:"sanitized,omitempty"`
	// Lossy 是出站编码因目标协议表达不了而丢弃的字段说明；为空表示无损转换。
	// 与 Sanitized 分列：前者指向客户端 bug，后者指向路由选型。
	Lossy []string `json:"lossy,omitempty"`
}

// RequestPage 的 NextCursor 为空表示没有下一页。
type RequestPage struct {
	Requests   []RequestSummary `json:"requests"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

type LivePage struct {
	// Entries 最新在前。
	Entries []LiveEntry `json:"entries"`
	// Degraded 为真表示缓存不可用，实时页是空的而非真的没有流量。
	Degraded bool `json:"degraded,omitempty"`
}

type LiveEntry struct {
	RequestID        string    `json:"request_id"`
	At               time.Time `json:"at"`
	InboundProtocol  string    `json:"inbound_protocol"`
	OutboundProtocol string    `json:"outbound_protocol,omitempty"`
	UserModel        string    `json:"user_model"`
	ModelID          string    `json:"model_id,omitempty"`
	Account          string    `json:"account,omitempty"`
	Outcome          string    `json:"outcome"`
	StatusCode       int       `json:"status_code,omitempty"`
	Attempts         int       `json:"attempts,omitempty"`
	Stream           bool      `json:"stream,omitempty"`
	LatencyMS        int       `json:"latency_ms,omitempty"`
	FirstTokenMS     int       `json:"first_token_ms,omitempty"`
	InputTokens      int64     `json:"input_tokens,omitempty"`
	OutputTokens     int64     `json:"output_tokens,omitempty"`
	ErrorCode        string    `json:"error_code,omitempty"`
}

type Stats struct {
	Window  string       `json:"window"`
	Buckets []StatBucket `json:"buckets"`
	Totals  StatTotals   `json:"totals"`
	// Degraded 为真表示统计来自缓存而缓存不可用，数字是空的而非真的为零。
	Degraded bool `json:"degraded,omitempty"`
}

type StatBucket struct {
	Minute       time.Time        `json:"minute"`
	Total        int64            `json:"total"`
	Outcomes     map[string]int64 `json:"outcomes,omitempty"`
	InputTokens  int64            `json:"input_tokens"`
	OutputTokens int64            `json:"output_tokens"`
	AvgLatencyMS int64            `json:"avg_latency_ms"`
}

type StatTotals struct {
	Total        int64            `json:"total"`
	Outcomes     map[string]int64 `json:"outcomes,omitempty"`
	InputTokens  int64            `json:"input_tokens"`
	OutputTokens int64            `json:"output_tokens"`
	// SuccessRate 是 normal 占比，0 到 1。总数为零时为 0。
	SuccessRate float64 `json:"success_rate"`
	// QPS 是窗口内的平均每秒请求数。
	QPS float64 `json:"qps"`
}

type OutboxEntry struct {
	ReportID      string    `json:"report_id"`
	RequestID     string    `json:"request_id"`
	ModelID       string    `json:"model_id"`
	Outcome       string    `json:"outcome"`
	Attempts      int       `json:"attempts"`
	NextAttemptAt time.Time `json:"next_attempt_at"`
	LastError     string    `json:"last_error,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

type OutboxPage struct {
	Entries []OutboxEntry `json:"entries"`
}

// ModelInfo 把用户模型与「本服务能否真的发给它」并列。
//
// OutboundReady 为假意味着调度层可能把请求路由到一个本服务没有实现出站
// 编解码的协议上，那会以 invalid_model 收场。让它在管理面直接可见，
// 而不是等到线上报错才发现。
type ModelInfo struct {
	Name          string `json:"name"`
	Collection    string `json:"collection"`
	Policy        string `json:"policy,omitempty"`
	Protocol      string `json:"protocol,omitempty"`
	Enabled       bool   `json:"enabled"`
	OutboundReady bool   `json:"outbound_ready"`
}

type ModelsPage struct {
	Models []ModelInfo `json:"models"`
	// Inbound 与 Outbound 是本服务实际装配了的协议，供前端解释 OutboundReady。
	Inbound  []string `json:"inbound"`
	Outbound []string `json:"outbound"`
	// Cached 为真表示清单来自缓存而非刚从调度层取的。
	Cached bool `json:"cached,omitempty"`
}
