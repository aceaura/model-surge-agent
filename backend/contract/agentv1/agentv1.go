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
	// Pool 缺省表示没有 PG 连接池（未配或已关）。
	//
	// 指针而非零值：报一组零会让运维看到 total=0, max=0 以为池配崩了去查
	// DSN，而真相是这个部署根本没配 PG。零是「池此刻空闲」的合法状态。
	Pool *PoolStats `json:"pool,omitempty"`
	// Goroutines 刻意不加 omitempty：NumGoroutine() 永远 >= 1，
	// 零值不可能出现，加了只会让读代码的人怀疑它会不会缺。
	Goroutines int `json:"goroutines"`
}

// PoolStats 是 PG 连接池的饱和度快照。整个服务变慢而每条流水都不异常时，
// 慢的那段在拿连接上，而那段不在任何一条请求的计时里。
type PoolStats struct {
	Acquired int32 `json:"acquired"`
	Idle     int32 `json:"idle"`
	Total    int32 `json:"total"`
	Max      int32 `json:"max"`
	// AcquireWaiting 是累计发生过「池空、只能等」的次数，不是此刻的排队长度。
	// 两次取样做差才是这段时间的等待次数。
	AcquireWaiting int64 `json:"acquire_waiting"`
}

// RequestSummary 是一条请求流水。绝不含凭据与对话内容。
type RequestSummary struct {
	RequestID       string    `json:"request_id"`
	At              time.Time `json:"at"`
	InboundProtocol string    `json:"inbound_protocol"`
	// Path 是客户端打的请求路径，不含 query。同一协议有多条别名共用一个
	// 处理函数，不看它就分不出客户端把 base_url 配成了哪一种。
	Path             string   `json:"path,omitempty"`
	OutboundProtocol string   `json:"outbound_protocol,omitempty"`
	UserModel        string   `json:"user_model"`
	ModelID          string   `json:"model_id,omitempty"`
	Account          string   `json:"account,omitempty"`
	Outcome          string   `json:"outcome"`
	StatusCode       int      `json:"status_code,omitempty"`
	Attempts         int      `json:"attempts,omitempty"`
	TriedIDs         []string `json:"tried_ids,omitempty"`
	Committed        bool     `json:"committed,omitempty"`
	Stream           bool     `json:"stream,omitempty"`
	UsageEstimated   bool     `json:"usage_estimated,omitempty"`
	InputTokens      int64    `json:"input_tokens,omitempty"`
	OutputTokens     int64    `json:"output_tokens,omitempty"`
	CacheReadTokens  int64    `json:"cache_read_tokens,omitempty"`
	// CacheWriteTokens 与 ReasoningTokens 与 ir.Usage 同名同义。客户端那侧本来
	// 就收到这两位，管理面不暴露会让排查成本问题的人以为上游没给。
	// 5m/1h 两位是缓存写入总量的 TTL 细分（anthropic cache_creation 明细），
	// 非 anthropic 来源恒为零。
	CacheWriteTokens   int64 `json:"cache_write_tokens,omitempty"`
	ReasoningTokens    int64 `json:"reasoning_tokens,omitempty"`
	CacheWrite5mTokens int64 `json:"cache_write_5m_tokens,omitempty"`
	CacheWrite1hTokens int64 `json:"cache_write_1h_tokens,omitempty"`
	// 托管工具次数（按次计费）与音频/预测明细四位：同 TTL 细分口径，
	// 非对应族来源恒为零。
	WebSearchRequests        int64 `json:"web_search_requests,omitempty"`
	WebFetchRequests         int64 `json:"web_fetch_requests,omitempty"`
	PromptAudioTokens        int64 `json:"prompt_audio_tokens,omitempty"`
	CompletionAudioTokens    int64 `json:"completion_audio_tokens,omitempty"`
	AcceptedPredictionTokens int64 `json:"accepted_prediction_tokens,omitempty"`
	RejectedPredictionTokens int64 `json:"rejected_prediction_tokens,omitempty"`
	LatencyMS                int   `json:"latency_ms,omitempty"`
	FirstTokenMS             int   `json:"first_token_ms,omitempty"`
	// DispatchMS、UpstreamMS 是两段跨进程耗时的累计值（含全部重试）。
	// latency_ms 减去两段即「本服务自身 + 上游生成」。
	DispatchMS   int    `json:"dispatch_ms,omitempty"`
	UpstreamMS   int    `json:"upstream_ms,omitempty"`
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	// Sanitized 是对客户端请求所做的畸形修复说明；为空表示请求本身合法。
	Sanitized []string `json:"sanitized,omitempty"`
	// Lossy 是出站编码因目标协议表达不了而丢弃的字段说明；为空表示无损转换。
	// 与 Sanitized 分列：前者指向客户端 bug，后者指向路由选型。
	Lossy []string `json:"lossy,omitempty"`
}

// AttemptTrailItem 是一次尝试的轨迹项。绝不含凭据、BaseURL 与请求体。
type AttemptTrailItem struct {
	// N 是尝试序号，从 1 起。不带 omitempty：零值本身就是 bug 信号。
	N int `json:"n"`
	// 调度层没给出目标的那次尝试，这三项为空——它确实没有目标。
	ModelID          string `json:"model_id,omitempty"`
	Account          string `json:"account,omitempty"`
	OutboundProtocol string `json:"outbound_protocol,omitempty"`
	Outcome          string `json:"outcome"`
	StatusCode       int    `json:"status_code,omitempty"`
	// DispatchMS、UpstreamMS 是**本次**尝试的耗时，不是累计值；
	// 各项之和等于行上的 dispatch_ms、upstream_ms。都不带 omitempty：
	// 0 是有意义的观测值，省掉会让「很快」与「没记」无法区分。
	DispatchMS   int       `json:"dispatch_ms"`
	UpstreamMS   int       `json:"upstream_ms"`
	ErrorCode    string    `json:"error_code,omitempty"`
	ErrorMessage string    `json:"error_message,omitempty"`
	RetryAfter   time.Time `json:"retry_after,omitzero"`
}

// RequestDetail 是单条流水的详情，比列表项多一条逐次尝试轨迹。
//
// 与 RequestSummary 分型而不是给后者加字段：列表一页最多 200 条，
// 每条再挂 N 项轨迹会让分页响应随重试次数膨胀，而列表的用途是找到那一条。
type RequestDetail struct {
	RequestSummary
	AttemptsTrail []AttemptTrailItem `json:"attempts_trail,omitempty"`
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
	DispatchMS       int       `json:"dispatch_ms,omitempty"`
	UpstreamMS       int       `json:"upstream_ms,omitempty"`
	InputTokens      int64     `json:"input_tokens,omitempty"`
	OutputTokens     int64     `json:"output_tokens,omitempty"`
	// 后五维与 /admin/requests 的明细列一一对应。少报它们等于少报计费权重
	// 最偏的那几维，而两个视图都不报错。
	CacheReadTokens    int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens   int64 `json:"cache_write_tokens,omitempty"`
	ReasoningTokens    int64 `json:"reasoning_tokens,omitempty"`
	CacheWrite5mTokens int64 `json:"cache_write_5m_tokens,omitempty"`
	CacheWrite1hTokens int64 `json:"cache_write_1h_tokens,omitempty"`
	// 托管次数与音频/预测明细：与明细列一一对应，少报即对不上账。
	WebSearchRequests        int64  `json:"web_search_requests,omitempty"`
	WebFetchRequests         int64  `json:"web_fetch_requests,omitempty"`
	PromptAudioTokens        int64  `json:"prompt_audio_tokens,omitempty"`
	CompletionAudioTokens    int64  `json:"completion_audio_tokens,omitempty"`
	AcceptedPredictionTokens int64  `json:"accepted_prediction_tokens,omitempty"`
	RejectedPredictionTokens int64  `json:"rejected_prediction_tokens,omitempty"`
	ErrorCode                string `json:"error_code,omitempty"`
	// LogPersisted 三态：nil 未配明细表、false 落库失败（这条不在
	// /admin/requests 里）、true 成功。布尔零值会让「没配」与「失败」同值。
	LogPersisted *bool `json:"log_persisted,omitempty"`
}

type Stats struct {
	Window  string       `json:"window"`
	Buckets []StatBucket `json:"buckets"`
	Totals  StatTotals   `json:"totals"`
	// Degraded 为真表示统计来自缓存而缓存不可用，数字是空的而非真的为零。
	Degraded bool `json:"degraded,omitempty"`
}

type StatBucket struct {
	Minute             time.Time        `json:"minute"`
	Total              int64            `json:"total"`
	Outcomes           map[string]int64 `json:"outcomes,omitempty"`
	InputTokens        int64            `json:"input_tokens"`
	OutputTokens       int64            `json:"output_tokens"`
	CacheReadTokens    int64            `json:"cache_read_tokens"`
	CacheWriteTokens   int64            `json:"cache_write_tokens"`
	ReasoningTokens    int64            `json:"reasoning_tokens"`
	CacheWrite5mTokens int64            `json:"cache_write_5m_tokens"`
	CacheWrite1hTokens int64            `json:"cache_write_1h_tokens"`
	// 托管次数与音频/预测明细：趋势图各占一列，不加权合计。
	WebSearchRequests        int64 `json:"web_search_requests"`
	WebFetchRequests         int64 `json:"web_fetch_requests"`
	PromptAudioTokens        int64 `json:"prompt_audio_tokens"`
	CompletionAudioTokens    int64 `json:"completion_audio_tokens"`
	AcceptedPredictionTokens int64 `json:"accepted_prediction_tokens"`
	RejectedPredictionTokens int64 `json:"rejected_prediction_tokens"`
	AvgLatencyMS             int64 `json:"avg_latency_ms"`
}

type StatTotals struct {
	Total              int64            `json:"total"`
	Outcomes           map[string]int64 `json:"outcomes,omitempty"`
	InputTokens        int64            `json:"input_tokens"`
	OutputTokens       int64            `json:"output_tokens"`
	CacheReadTokens    int64            `json:"cache_read_tokens"`
	CacheWriteTokens   int64            `json:"cache_write_tokens"`
	ReasoningTokens    int64            `json:"reasoning_tokens"`
	CacheWrite5mTokens int64            `json:"cache_write_5m_tokens"`
	CacheWrite1hTokens int64            `json:"cache_write_1h_tokens"`
	// 与 StatBucket 同列：窗口合计。
	WebSearchRequests        int64 `json:"web_search_requests"`
	WebFetchRequests         int64 `json:"web_fetch_requests"`
	PromptAudioTokens        int64 `json:"prompt_audio_tokens"`
	CompletionAudioTokens    int64 `json:"completion_audio_tokens"`
	AcceptedPredictionTokens int64 `json:"accepted_prediction_tokens"`
	RejectedPredictionTokens int64 `json:"rejected_prediction_tokens"`
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

// CaptureSummary 是捕获列表里的一条，只给标识与四体长度，不带字节。
//
// 列表不回字节：一条捕获可达数 MB，列 32 条就是上百 MB 的响应。
// 先看列表挑出可疑的那条，再单独取它的四体。
type CaptureSummary struct {
	RequestID string    `json:"request_id"`
	At        time.Time `json:"at"`
	// Sizes 是四体各自已捕获的字节数，键为 client_request、upstream_request、
	// upstream_response、client_response。某一体缺失时它的值为 0。
	Sizes map[string]int `json:"sizes"`
}

// CaptureBody 是四体中的一体。
type CaptureBody struct {
	// Body 是原始 wire 字节，按 UTF-8 当字符串交出，不做 base64。
	//
	// 这个端点唯一的用途是人眼看「哪一步坏了」，base64 之后要先解一层
	// 才能看，那就把它从一个能直接用的排查设施降级成一个需要工具的。
	Body string `json:"body"`
	// Truncated 为真表示超出上限被截断，保留的是**前段**。
	Truncated bool `json:"truncated"`
	// Dropped 是被截断掉的字节数。
	Dropped int `json:"dropped"`
}

// CaptureDetail 是一次请求的四体全文。
//
// 绝不含任何请求头：上游凭据只存在于 http.Request 里，
// 让它进捕获等于把排查设施变成凭据泄露面。
type CaptureDetail struct {
	RequestID        string      `json:"request_id"`
	At               time.Time   `json:"at"`
	ClientRequest    CaptureBody `json:"client_request"`
	UpstreamRequest  CaptureBody `json:"upstream_request"`
	UpstreamResponse CaptureBody `json:"upstream_response"`
	ClientResponse   CaptureBody `json:"client_response"`
}

// CaptureList 是捕获列表。Mode 一并给出，让列表为空时能区分
// 「捕获关着」与「开着但还没有符合条件的请求」。
type CaptureList struct {
	Mode  string           `json:"mode"`
	Items []CaptureSummary `json:"items"`
}
