package relayclient

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// 这些类型镜像 model-surge-relay 的 contract/relayv1，字段名必须逐字一致。
// 不直接 import 那个包：两个服务独立发版，跨仓 import 会把调度层的
// 内部依赖（apperr、upstreamclient）拖进数据面。

// 调度面路径，前缀与本服务对客户端暴露的 /v1 同名但无关：
// 这几条是**出站**打到 relay 的地址，不是本服务的入站路由。
const (
	PathDispatch = "/v1/dispatch"
	PathResults  = "/v1/results"
	PathModels   = "/v1/models"
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
	// CacheWriteTokens 与 ReasoningTokens 与 ir.Usage 同名同义。
	//
	// 两位都必须过这条边界：客户端那侧确实收到了它们（anthropic 出站把缓存写入
	// 填进 cache_creation_input_tokens），所以「上报里没有」不是「上游没给」，
	// 而是本服务把算出来的数字在跨进程时丢了一半，两边对账永久差额。
	//
	// 调度层的 runstate 仍只累计前三位：缓存写入与推理的单价与输入输出不同，
	// 直接加进同一组累计列等于用错的权重记账，而加权需要定价模型（在 upstream
	// 配置中心）。这里如实交出去，让将来做分档定价时数据已经在库里。
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  int64 `json:"reasoning_tokens,omitempty"`
	// CacheWrite5m/1hTokens 是缓存写入总量的 TTL 细分（anthropic 上游的
	// cache_creation 明细），1h 档单价通常是 5m 的 2 倍，分档定价要靠它。
	// 与上面两位同理如实交出去；调度层不认识这两个键也无妨——runstate
	// 只累计前三位，多出来的键被忽略，本服务的 request_log 照记。
	// IR 侧的「明细已知」标记不过这条边界：下游拿到零值分不清真零还是
	// 未知，但记账只认非零数，语义无损。
	CacheWrite5mTokens int64 `json:"cache_write_5m_tokens,omitempty"`
	CacheWrite1hTokens int64 `json:"cache_write_1h_tokens,omitempty"`
	// WebSearch/WebFetchRequests 是服务端托管工具的执行次数（anthropic 的
	// usage.server_tool_use），按次计费；音频与预测加速四位是 chat 的
	// usage 明细子集。同 TTL 细分的口径：如实交出去，调度层不认识这些键
	// 也无妨，runstate 只累计前三位，多出来的键被忽略。
	WebSearchRequests        int64 `json:"web_search_requests,omitempty"`
	WebFetchRequests         int64 `json:"web_fetch_requests,omitempty"`
	PromptAudioTokens        int64 `json:"prompt_audio_tokens,omitempty"`
	CompletionAudioTokens    int64 `json:"completion_audio_tokens,omitempty"`
	AcceptedPredictionTokens int64 `json:"accepted_prediction_tokens,omitempty"`
	RejectedPredictionTokens int64 `json:"rejected_prediction_tokens,omitempty"`
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
	// OutcomeTransport 出站连接层故障：不计入失败计数，运行态零变更。
	//
	// 与 context_exceeded 的零变更语义相同但刻意不复用它：一个是「请求太大」、
	// 一个是「我们的连接坏了」，合成一类会让运维在流水里分不开两种成因
	// 完全不同的故障。
	OutcomeTransport = "transport"
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
	// RetryAfter 是上游明示的「这个目标最早什么时候能再用」。
	//
	// 零值（键不出现）表示上游没说，调度层回落到自己的失败计数启发式。
	// 非零时调度层应当直接冷却到该时刻：上游的明示比启发式可靠，
	// 按默认时长猜会让我们在整个限流窗口里反复空转。
	//
	// 传时刻而非时长：跨进程的排队与网络往返会让时长失真。
	RetryAfter time.Time `json:"retry_after,omitzero"`
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
