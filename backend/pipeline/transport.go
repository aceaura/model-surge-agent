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
	// H2SendPingTimeout 是 h2 连接空闲多久之后发一个 PING 去探。
	// H2PingTimeout 是 PING 发出后多久没收到 PONG 就判连接失联。
	// 任一为负表示显式关闭探测。
	H2SendPingTimeout time.Duration
	H2PingTimeout     time.Duration
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
	// h2 死连接探测。最坏检出耗时是两者之和（刚探完就变死 → 等一个
	// SendPing 才发下一个 PING → 再等一个 Ping 判失联）。
	//
	// 上界：和为 30s，必须显著小于 ResponseHeaderTimeout 的 120s，
	// 否则被动超时先触发、ping 等于没配。本机探针实测：不配 ping 时撞上
	// 静默黑洞的请求挂到 20s 的 ctx 超时都不失败，配了则 4s 内明确失败。
	//
	// 下界：不取 sub2api 的 10s/5s。PING 走 h2 连接层、与流数据无关，
	// 健康连接一定回 PONG，所以压到 5s 不会误杀长流——但一条长流的生命周期里
	// 要发几十个 PING，换来的只是检出快十几秒。
	defaultH2SendPingTimeout = 15 * time.Second
	defaultH2PingTimeout     = 15 * time.Second
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

	// h2 死连接探测。HTTPS 上游因 ForceAttemptHTTP2 实际走 h2，而一条 h2
	// 连接承载全部多路复用的流，它静默死掉会拖垮所有并发请求，
	// 且不主动探就只能等被动超时——那对一次尝试是整个重试预算。
	//
	// 不改 ForceAttemptHTTP2：是否走 h2 由 TLS 协商决定，
	// 这里只管走了之后死连接能不能被探出来。
	send := durationOrDefault(opts.H2SendPingTimeout, defaultH2SendPingTimeout)
	pong := durationOrDefault(opts.H2PingTimeout, defaultH2PingTimeout)
	// 任一被显式关掉（负值经 durationOrDefault 变成 0）就整个不配：
	// 只配一半是无意义的状态——光有探测间隔没有失联判定，PING 发出去
	// 永远等不到结论。
	if send > 0 && pong > 0 {
		t.HTTP2 = &http.HTTP2Config{SendPingTimeout: send, PingTimeout: pong}
	}

	// 不设 Client.Timeout：它覆盖到读完整个响应体，而 SSE 会跑几分钟。
	// 设了就会从中间掐断，且掐断点落在已 committed 之后，
	// 客户端收到的是一个残缺的流。流的时限由首帧与空闲两个计时器负责。
	//
	// CheckRedirect 必须显式设：默认策略会把 302 的 POST 改写成无体的 GET，
	// 并且跨 host 时只删它认识的那四个头名——本服务的 x-api-key 与
	// x-goog-api-key 不在其中。
	return &http.Client{Transport: t, CheckRedirect: checkRedirect}
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
