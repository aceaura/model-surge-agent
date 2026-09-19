package ir

import (
	"fmt"
	"net/http"
	"time"
)

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
	// ErrTransport 出站连接层故障：连接被重置、h2 连接判定失联、拨号或握手失败。
	//
	// 与 ErrUpstream 分开：后者是「上游收到了请求并明确地不行」，而这一类里
	// 上游可能完全健康——坏的是我们连接池里那条连接。混成一类会让一条死连接
	// 把健康账号的失败计数推向冷却，而它本该换条连接就好。
	ErrTransport ErrorKind = "transport"
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
	// RetryAfter 是上游明示的「最早可以再来」的绝对时刻，零值表示上游没说。
	//
	// 存时刻而不是时长：这个值要跨进程传到调度层，时长会在排队与网络往返里
	// 失真，时刻不会。同样不进 NewError 的参数表——只有上游错误有这一维度。
	//
	// 绝不编造：没给就是零值。编造一个时刻会把其实可用的目标锁住。
	RetryAfter time.Time `json:"retry_after,omitzero"`
	// SideEffectRisk 表示这次失败发生在请求已经完整交给上游之后。
	//
	// 与 Retryable 分开而不是直接把它压成 false：两者回答不同的问题。
	// Retryable 是「换个目标有没有意义」，这一个是「换个目标会不会产生
	// 第二份计费」。压成一个字段后，将来要放宽某一类（比如上游明确说了
	// 「我没开始处理」）就没有可放宽的地方。
	//
	// 不进 NewError 的参数表：绝大多数错误产生在请求发出之前，
	// 加进签名等于让四十余处调用点都跟着填一个 false。
	SideEffectRisk bool `json:"side_effect_risk,omitempty"`
	// ForwardHeaders 是这次失败的上游响应里可以回传给客户端的那些头
	// （限流剩余量那一族）。不序列化：它只在进程内从建流点传到终态写出点，
	// 既不进流水也不进上报——那两处要的是标量结论，不是一份响应头副本。
	//
	// 挂在错误上而不是给终态写出函数多传一个上游句柄：错误值本身已经贯穿
	// 建流到终态的全程（RetryAfter 与 SideEffectRisk 走的就是这条路），
	// 而中间隔着三层调用与五条终止分支，多传一个参数意味着五条分支都要
	// 跟改，漏一条不会变红。
	ForwardHeaders http.Header `json:"-"`
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
	case ErrRateLimit, ErrUpstream, ErrTimeout, ErrTransport:
		return true
	default:
		return false
	}
}
