package relayclient

import (
	"fmt"
	"net/http"
	"time"
)

// Options 是控制面客户端的可调参数。
// 零值取内置默认；两个超时取负值表示显式不设限。
type Options struct {
	MaxIdleConns        int
	MaxIdleConnsPerHost int
	IdleConnTimeout     time.Duration
	// ResponseHeaderTimeout 只约束「发出请求到响应头到达」这一段。
	ResponseHeaderTimeout time.Duration
	// Timeout 是整次调用的总时限，覆盖到读完正文。
	Timeout time.Duration
	// Proxy 决定控制面是否使用环境变量里的代理。
	Proxy ProxyMode
}

// 控制面连接层默认值。
//
// 与出站数据面（pipeline.TransportOptions）刻意不共用一套：那一层要容忍推理模型
// 在首帧前思考几分钟，而控制面每次只是内网的一个小 JSON 往返。把两层「统一」成
// 同一组默认值，等于用数据面的宽松度去等一个本该秒回的内部查询。
const (
	// 控制面只打一个 host，不需要数据面的 256。
	defaultMaxIdleConns = 64
	// 必须显式设：不设则取 http.DefaultMaxIdleConnsPerHost = 2。本机探针实测
	// 并发 8 做两轮共 16 次请求，拨号 16 次、复用 0 次——而控制面的全部请求都
	// 打向同一个 relay host，正是 PerHost 卡得最死的形态。
	//
	// 取值与数据面同为 32：两层承载的是同一批请求（每次数据面调用先问一次
	// dispatch），并发上限由数据面决定，设小了一样会退化成每请求新建连接。
	defaultMaxIdleConnsPerHost = 32
	// 须小于上游 LB 的空闲回收时间，否则复用到的是对端已经关掉的连接。
	defaultIdleConnTimeout = 90 * time.Second
	// 远紧于数据面的 120s：控制面是内网的小 JSON 查询，没有「上游攒够一批才
	// 发响应头」那一形态。设宽了则「relay 连上但不回头」这类故障要等到总超时
	// 才暴露，而那时错误文本与「调用方取消」无法区分。
	defaultResponseHeaderTimeout = 10 * time.Second
	// 总时限，兜「响应头到了但正文读不完」。必须宽于 ResponseHeaderTimeout，
	// 否则先触发的是它，分层就白设了。
	defaultTimeout = 30 * time.Second
)

// ProxyMode 是控制面的代理策略。
type ProxyMode string

const (
	// ProxyOff 不使用环境变量里的代理。这是默认值。
	//
	// 默认关而不是沿用标准库的 ProxyFromEnvironment：控制面的 baseURL 在集群里
	// 是服务名（compose 下形如 http://modelsurge-replay:18101），本机探针实测
	// 这类主机名会命中 HTTP_PROXY，只有 127.0.0.1 与 localhost 被内置规则排除。
	// 而宿主机配了出网代理是常态——默认开会让一个本来正常的部署静默坏掉，
	// 且症状是「relay 不可达」，看不出根因在代理上。
	ProxyOff ProxyMode = "off"
	// ProxyEnvironment 按 HTTP_PROXY/HTTPS_PROXY/NO_PROXY 走代理。
	// 给 relay 确实在另一个网络域、必须经代理才能到达的部署。
	ProxyEnvironment ProxyMode = "environment"
)

// ParseProxyMode 解析代理策略。空串取默认。
func ParseProxyMode(s string) (ProxyMode, error) {
	switch ProxyMode(s) {
	case "":
		return ProxyOff, nil
	case ProxyOff:
		return ProxyOff, nil
	case ProxyEnvironment:
		return ProxyEnvironment, nil
	}
	return "", fmt.Errorf("unknown proxy mode %q (want %q or %q)", s, ProxyOff, ProxyEnvironment)
}

// newTransport 构造控制面的传输层。
//
// 从 DefaultTransport.Clone() 起手：默认那份带着拨号超时与 keep-alive、
// ForceAttemptHTTP2、TLS 握手超时。新建一个空 Transport 会把这些全丢掉。
func newTransport(opts Options) *http.Transport {
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// 理论上不会发生。真发生时返回一个空 Transport 而不是 nil：
		// nil 会让 http.Client 回落 DefaultTransport，等于这一层静默失效。
		return &http.Transport{}
	}
	t := tr.Clone()

	t.MaxIdleConns = intOrDefault(opts.MaxIdleConns, defaultMaxIdleConns)
	t.MaxIdleConnsPerHost = intOrDefault(opts.MaxIdleConnsPerHost, defaultMaxIdleConnsPerHost)
	t.IdleConnTimeout = durationOrDefault(opts.IdleConnTimeout, defaultIdleConnTimeout)
	t.ResponseHeaderTimeout = durationOrDefault(opts.ResponseHeaderTimeout, defaultResponseHeaderTimeout)

	// Clone 带过来的是 ProxyFromEnvironment，默认要清掉。
	if opts.Proxy != ProxyEnvironment {
		t.Proxy = nil
	}
	return t
}

func intOrDefault(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// durationOrDefault 零值取默认、负值表示显式不设限。
//
// 负值而非零值表示不设限：零值是「环境变量没配」的自然形态，让它表示不设限
// 会让忘配的部署静默回到无限等待。
func durationOrDefault(v, def time.Duration) time.Duration {
	if v < 0 {
		return 0
	}
	if v == 0 {
		return def
	}
	return v
}
