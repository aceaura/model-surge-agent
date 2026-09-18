package pipeline

import (
	"net/http"
	"time"
)

// TransportOptions 是出站连接层的可调参数。
// 零值取内置默认；两个超时取负值表示显式不设限。
type TransportOptions struct {
	MaxIdleConns        int
	MaxIdleConnsPerHost int
	IdleConnTimeout     time.Duration
	// ResponseHeaderTimeout 只约束「发出请求到响应头到达」这一段。
	ResponseHeaderTimeout time.Duration
}

// 连接层默认值。
//
// PerHost 必须显式设：不设则取 http.DefaultMaxIdleConnsPerHost = 2，
// 探针实测并发 8 连做两轮共 16 次请求只能复用 2 次、新建 14 条连接，
// 而每条新连接对 HTTPS 上游都是一次完整 TLS 握手。
//
// 总量给到 PerHost 的若干倍：账号池横跨多个上游 host，
// 总量卡太死会让 PerHost 白设。
const (
	defaultMaxIdleConns        = 256
	defaultMaxIdleConnsPerHost = 32
	defaultIdleConnTimeout     = 90 * time.Second
	// 响应头等待必须宽于首帧超时（默认 60s）：推理模型在首帧前会思考很久，
	// 而有些兼容层网关攒够一批内容才发响应头。设得比首帧超时还短，
	// 会把本该由首帧超时报出的故障错报成连接层问题。
	defaultResponseHeaderTimeout = 120 * time.Second
)

// NewHTTPClient 构造调用上游的 HTTP 客户端。
//
// 从 DefaultTransport.Clone() 起手而不是新建一个空 Transport：默认那份带着
// ProxyFromEnvironment、拨号超时与 keep-alive、ForceAttemptHTTP2、
// TLS 握手超时。新建会把这些全丢掉，其中丢 Proxy 最致命——需要走代理才能
// 出网的部署会直接连不上，而症状是「上游不可达」，看不出根因在这里。
//
// Clone 而非改 DefaultTransport 本体：后者会污染 relayclient 与任何用
// 默认客户端的代码，那是进程级的隐蔽副作用。
func NewHTTPClient(opts TransportOptions) *http.Client {
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// 理论上不会发生。真发生时也不能返回 nil：调用方会回落
		// DefaultClient，等于这一整层配置静默失效。
		return &http.Client{}
	}
	t := tr.Clone()

	t.MaxIdleConns = intOrDefault(opts.MaxIdleConns, defaultMaxIdleConns)
	t.MaxIdleConnsPerHost = intOrDefault(opts.MaxIdleConnsPerHost, defaultMaxIdleConnsPerHost)
	t.IdleConnTimeout = durationOrDefault(opts.IdleConnTimeout, defaultIdleConnTimeout)
	// 只约束「发出请求到响应头到达」这一段，头到了之后读正文不受它影响。
	// 这正是要挡的那类故障：上游接受了连接却永不回头，既不超时也不报错，
	// 而首帧超时的计时器要等建流之后才起，管不到这里。
	t.ResponseHeaderTimeout = durationOrDefault(opts.ResponseHeaderTimeout, defaultResponseHeaderTimeout)

	// 不设 Client.Timeout：它覆盖到读完整个响应体，而 SSE 会跑几分钟。
	// 设了就会从中间掐断，且掐断点落在已 committed 之后，
	// 客户端收到的是一个残缺的流。流的时限由首帧与空闲两个计时器负责。
	return &http.Client{Transport: t}
}

func intOrDefault(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// durationOrDefault 零值取默认、负值表示显式不设限。
//
// 负值而非零值表示「不设限」：零值是「环境变量没配」的自然形态，
// 让它表示不设限会让忘配的部署静默回到无限等待，而那正是本轮要修的故障。
// 想退回旧行为的运维配一个负值，那是明确的意思表示。
//
// 不做溢出钳位：参考实现（new-api service/http_client.go:108-116）收的是秒数，
// 秒数乘 time.Second 会回绕成极小的正值，把每个请求都掐断，所以它必须钳。
// 我们收的是 Duration，由 time.ParseDuration 解析——探针实测它对超出
// int64 纳秒的输入直接报错（"2562048h" → invalid duration），
// 非法值会进启动错误一次报全，回绕不可能发生。
func durationOrDefault(v, def time.Duration) time.Duration {
	if v < 0 {
		return 0
	}
	if v == 0 {
		return def
	}
	return v
}
