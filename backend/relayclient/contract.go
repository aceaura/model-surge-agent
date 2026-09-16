package relayclient

import (
	"encoding/json"
	"fmt"
	"strings"
)

// 这些类型镜像 model-surge-relay 的 contract/relayv1，字段名必须逐字一致。
// 不直接 import 那个包：两个服务独立发版，跨仓 import 会把调度层的
// 内部依赖（apperr、upstreamclient）拖进数据面。

const (
	PathDispatch = "/internal/v1/dispatch"
	PathResults  = "/internal/v1/results"
	PathModels   = "/internal/v1/models"
)

type DispatchRequest struct {
	Model           string   `json:"model"`
	InboundProtocol string   `json:"inbound_protocol"`
	ClientKey       string   `json:"client_key"`
	RequestID       string   `json:"request_id"`
	TriedIDs        []string `json:"tried_ids,omitempty"`
	EstTokens       int      `json:"est_tokens,omitempty"`
}

// Target 是选中 upstream model 的全套信息。
//
// Headers 含认证头：只驻内存、只写进出站 http.Request，禁止入库或落日志。
// 打日志用 String()，它已脱敏。
//
// Defaults 与 Overrides 是两层未合并的参数，语义不同（前者缺失才填、
// 后者强制压盖），且写的是**出站协议的原生字段名**，因此要在编码出
// wire body 之后才逐层作用上去。
type Target struct {
	ModelID       string            `json:"model_id"`
	Account       string            `json:"account"`
	ProviderID    string            `json:"provider_id"`
	Protocol      string            `json:"protocol"`
	BaseURL       string            `json:"base_url"`
	NativeModel   string            `json:"native_model"`
	ContextWindow int               `json:"context_window,omitempty"`
	Headers       map[string]string `json:"headers"`
	Defaults      json.RawMessage   `json:"defaults,omitempty"`
	Overrides     json.RawMessage   `json:"overrides,omitempty"`
}

func (t Target) String() string {
	return fmt.Sprintf("Target{ModelID:%q Account:%q Protocol:%q BaseURL:%q NativeModel:%q Headers:%v}",
		t.ModelID, t.Account, t.Protocol, t.BaseURL, t.NativeModel, RedactHeaders(t.Headers))
}

// sensitiveHeaders 是打日志时要遮蔽值的头（大小写不敏感）。
var sensitiveHeaders = map[string]bool{
	"authorization":  true,
	"x-api-key":      true,
	"x-goog-api-key": true,
}

// RedactHeaders 遮蔽凭据值，保留键名以便排查配置问题。
func RedactHeaders(h map[string]string) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if sensitiveHeaders[strings.ToLower(k)] {
			out[k] = mask(v)
			continue
		}
		out[k] = v
	}
	return out
}

func mask(v string) string {
	if len(v) <= 4 {
		return "****"
	}
	return "****" + v[len(v)-4:]
}

type Skip struct {
	ModelID string `json:"model_id"`
	Reason  string `json:"reason"`
	Detail  string `json:"detail,omitempty"`
}

type Decision struct {
	Policy        string   `json:"policy,omitempty"`
	PolicyVersion int      `json:"policy_version,omitempty"`
	Collection    string   `json:"collection"`
	Group         string   `json:"group"`
	GroupType     string   `json:"group_type"`
	Note          string   `json:"note,omitempty"`
	Candidates    []string `json:"candidates"`
	Skipped       []Skip   `json:"skipped,omitempty"`
}

type DispatchResponse struct {
	RequestID string   `json:"request_id"`
	Target    Target   `json:"target"`
	Decision  Decision `json:"decision"`
}

func (r DispatchResponse) String() string {
	return fmt.Sprintf("DispatchResponse{RequestID:%q Target:%s Decision:%+v}",
		r.RequestID, r.Target, r.Decision)
}

type Usage struct {
	InputTokens     int64 `json:"input_tokens,omitempty"`
	OutputTokens    int64 `json:"output_tokens,omitempty"`
	CacheReadTokens int64 `json:"cache_read_tokens,omitempty"`
}

// Outcome 决定调度层如何更新运行态，语义由 relay 的 runstate 定义。
const (
	// OutcomeNormal 清零失败计数并累计用量。
	OutcomeNormal = "normal"
	// OutcomeAbnormal 累计失败，可能触发冷却。
	OutcomeAbnormal = "abnormal"
	// OutcomeRetrying 同 abnormal，但表示本次会继续换目标。
	OutcomeRetrying = "retrying"
	// OutcomeInvalidModel 目标本身不可用（上游 404、无对应出站 codec）。
	OutcomeInvalidModel = "invalid_model"
	// OutcomeContextExceeded 完全不改运行态：输入太长是客户端的问题，
	// 不该记作这个目标的失败。
	OutcomeContextExceeded = "context_exceeded"
)

type ResultReport struct {
	ReportID  string `json:"report_id"`
	RequestID string `json:"request_id"`
	ModelID   string `json:"model_id"`
	Outcome   string `json:"outcome"`
	Usage     Usage  `json:"usage,omitempty"`
}

type ReportResponse struct {
	Applied bool `json:"applied"`
}

type UserModelSummary struct {
	Name       string `json:"name"`
	Collection string `json:"collection"`
	Policy     string `json:"policy,omitempty"`
	Protocol   string `json:"protocol,omitempty"`
	Enabled    bool   `json:"enabled"`
}

type ModelsResponse struct {
	Models []UserModelSummary `json:"models"`
}

// relay 的错误码。可重试的那几个意味着换目标或稍后再试可能成功。
const (
	CodeUnauthorized      = "unauthorized"
	CodeNotFound          = "not_found"
	CodeInvalidRequest    = "invalid_request"
	CodeTargetUnavailable = "target_unavailable"
	CodePolicyTimeout     = "policy_timeout"
	CodeInternal          = "internal_error"
)

type errorEnvelope struct {
	Error Error `json:"error"`
}

// Error 是 relay 返回的错误。Retryable 由 relay 判定，本服务照用不重新推断。
type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	Field     string `json:"field,omitempty"`
}

func (e *Error) Error() string { return "relay: " + e.Code + ": " + e.Message }

// Exhausted 表示候选已耗尽：重试循环见到它必须停下，
// 再调一次 dispatch 只会得到同样的答案。
func (e *Error) Exhausted() bool { return e.Code == CodeTargetUnavailable }
