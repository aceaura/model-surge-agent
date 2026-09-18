package ir

import "fmt"

// ErrorKind 是跨协议归一后的错误类别，决定重试与上报 outcome。
type ErrorKind string

const (
	// ErrInvalidRequest 请求本身有问题，换目标也没用。
	ErrInvalidRequest ErrorKind = "invalid_request"
	// ErrAuth 凭据无效或权限不足。
	ErrAuth ErrorKind = "authentication"
	// ErrNotFound 模型不存在。
	ErrNotFound ErrorKind = "not_found"
	// ErrRateLimit 限流，换目标有意义。
	ErrRateLimit ErrorKind = "rate_limit"
	// ErrContextExceeded 输入超出模型上下文窗口。换目标通常也超，
	// 且不应算作目标的失败，故单独成类。
	ErrContextExceeded ErrorKind = "context_exceeded"
	// ErrUpstream 上游 5xx 或响应无法解码。
	ErrUpstream ErrorKind = "upstream"
	// ErrTimeout 首字节或空闲超时。
	ErrTimeout ErrorKind = "timeout"
	// ErrInternal 本服务自身出错。
	ErrInternal ErrorKind = "internal"
	// ErrCanceled 客户端自己取消了请求。既不是目标的故障也不是本服务的错，
	// 单独成类是为了让运维能把它与真实上游故障分开统计——混在一起的话，
	// 客户端多按几次停止就会让健康账号的失败计数涨到冷却。
	ErrCanceled ErrorKind = "canceled"
)

// Error 是归一化的错误。StatusCode 是上游原始状态码（本地错误为 0），
// Retryable 表示换目标重试是否有意义。
type Error struct {
	Kind       ErrorKind `json:"kind"`
	StatusCode int       `json:"status_code,omitempty"`
	// Code 与 Message 尽量保留上游原文，便于客户端排查。
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
	// Param 指出是哪个请求字段有问题，只有 openai 系协议的错误信封有这一维度。
	// 归一时保留而非丢弃：这是 400 错误里最有排查价值的信息，客户端拿不到它
	// 就只能逐个字段试。不进 NewError 的参数表——绝大多数调用点是本地错误、
	// 没有这个维度，加进签名等于让四十余处调用点都跟着填一个空串。
	Param string `json:"param,omitempty"`
	// Retryable 由 Kind 决定，构造时统一填充。
	Retryable bool `json:"retryable"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Code != "" {
		return fmt.Sprintf("%s: %s (%s)", e.Kind, e.Message, e.Code)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Message)
}

// NewError 按 Kind 推导 Retryable，避免调用点各自判断导致不一致。
func NewError(kind ErrorKind, status int, code, message string) *Error {
	return &Error{
		Kind:       kind,
		StatusCode: status,
		Code:       code,
		Message:    message,
		Retryable:  retryable(kind),
	}
}

// retryable 只回答「换个目标再试有没有意义」。
// context_exceeded 不可重试：换目标大概率同样超限，且它不该算目标的失败。
func retryable(kind ErrorKind) bool {
	switch kind {
	case ErrRateLimit, ErrUpstream, ErrTimeout:
		return true
	default:
		return false
	}
}
