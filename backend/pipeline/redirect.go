package pipeline

import (
	"context"
	"errors"
	"net/http"
	"sync"
)

// 三个哨兵。全部归 ir.ErrUpstream 且可重试：重定向是这个目标的路由配置问题，
// 换目标有意义，而换连接没有——所以刻意不归 ErrTransport，那个 kind 的语义是
// 「换条连接有意义」，还会影响账号该不该冷却的归因。
var (
	errRedirectRewritesMethod = errors.New("redirect rewrites the request method")
	errRedirectCrossHost      = errors.New("redirect leaves the original host")
	errRedirectTooManyHops    = errors.New("redirect chain too long")
)

// maxRedirectHops 不取标准库的 10，也不做可配置。
//
// 上游对一个推理请求的正当重定向只有「同 host 换路径」一种形态，一次就够；
// 给到 3 是给「版本迁移又叠一层前缀」这种链留余量。上限存在的意义是防回环，
// 具体取几没有运维要调的理由。
const maxRedirectHops = 3

// redirectStrippedHeaders 是跨 host 时必须摘掉的头。
//
// 标准库自己只删 Authorization/Www-Authenticate/Cookie/Cookie2，而本服务的凭据
// 恰好是 x-api-key（Anthropic）与 x-goog-api-key（Gemini）——探针实测跨 host 时
// 这两个照带。Authorization 与 Cookie 仍然列上：这个集合的正确性不该依赖标准库
// 某个版本内部函数的实现。
var redirectStrippedHeaders = []string{
	"Authorization",
	"x-api-key",
	"x-goog-api-key",
	"api-key",
	"Cookie",
}

// redirectSink 收集策略函数产生的说明。
//
// 挂在 ctx 上而不是做成策略函数的字段：客户端是进程级共享的，CheckRedirect 是
// 它的一个字段，函数里存本次请求的状态会让并发请求互相串。探针实测重定向请求
// 继承原请求的 context，所以 ctx 是唯一能按请求分开的载体。
//
// 加锁不是因为一次请求内会并发（重定向是串行的），而是因为写发生在标准库的
// goroutine 里、读发生在我们这边，两者之间没有别的同步点。
type redirectSink struct {
	mu    sync.Mutex
	notes []string
}

func (s *redirectSink) add(note string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notes = append(s.notes, note)
}

func (s *redirectSink) drain() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.notes
	s.notes = nil
	return out
}

type redirectSinkKey struct{}

func withRedirectSink(ctx context.Context) (context.Context, *redirectSink) {
	s := &redirectSink{}
	return context.WithValue(ctx, redirectSinkKey{}, s), s
}

func sinkFrom(ctx context.Context) *redirectSink {
	s, _ := ctx.Value(redirectSinkKey{}).(*redirectSink)
	return s
}

// checkRedirect 是出站腿的重定向策略。
//
// 三条判断的顺序有意义：先删凭据、再判跳数、最后判方法改写。删除排在所有拒绝
// 之前是纵深防御——本轮的结论是跨 host 一律拒绝，所以删了之后并不会真的发出去；
// 但将来有人把 host 判据放宽时，删除已经在那儿了，凭据不会静默跟着走。
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		// 标准库不会这么调。真发生时放行：这个函数的职责是拒绝坏形态，
		// 而不是在拿不到基准时凭空报错。
		return nil
	}
	// 比最初那个请求而不是上一跳：A→B→A 的链条上逐跳比会认为最后一跳「回到了
	// 同 host」从而把凭据加回去，而凭据在 B 那一跳已经暴露过了。
	origin := via[0].URL.Host
	if req.URL.Host != origin {
		s := sinkFrom(req.Context())
		for _, name := range redirectStrippedHeaders {
			if req.Header.Get(name) == "" {
				continue
			}
			req.Header.Del(name)
			if s != nil {
				// 刻意不拼头的值，也不拼目标 URL：这条说明进流水的 lossy，
				// 而重定向目标 URL 可能带 query 里的 key。
				s.add("stripped credential header " +
					http.CanonicalHeaderKey(name) + " on cross-host redirect")
			}
		}
		return errRedirectCrossHost
	}

	if len(via) > maxRedirectHops {
		// 不让标准库的上限先触发：它的错误文本里带完整 URL 含 query
		// （探针实测 `stopped after 10 redirects` 那条）。
		return errRedirectTooManyHops
	}

	// 301/302/303 三个码都会被标准库改写成 GET 并丢掉请求体，而它在调用本函数
	// 之前就已经改好了——via 里留的是改写前的方法。比较方法覆盖三个码，
	// 且天然放过 307/308（它们不改写）。刻意不从方法反推状态码再按码判断：
	// 状态码不在手里，中间多一层猜测而结论完全相同。
	if req.Method != via[len(via)-1].Method {
		return errRedirectRewritesMethod
	}
	return nil
}

// redirectFailure 把策略拒绝渲染成一句固定文本。
//
// 固定文本而非从原始错误里取：Do 返回的错误是 *url.Error，它内嵌重定向目标
// URL 含 query。sanitizeTransportError 的兜底扫描虽然也能去掉 URL，但那是兜底；
// 这条路径上我们确切知道该说什么。
func redirectFailure(err error) (string, bool) {
	switch {
	case errors.Is(err, errRedirectRewritesMethod):
		// 点出「改写了方法」而不是笼统的上游失败：请求体被丢掉之后上游回的
		// 4xx 会被归成模型失败，把一个健康账号推向冷却，而原因在路由配置。
		return "upstream redirected in a way that rewrites the request method", true
	case errors.Is(err, errRedirectCrossHost):
		return "upstream redirected to a different host", true
	case errors.Is(err, errRedirectTooManyHops):
		return "upstream redirect chain was too long", true
	}
	return "", false
}

// unfollowableRedirect 是 3xx 走到状态码闸门时的归因。
//
// 能跟随的重定向不会走到那里——标准库跟随完才返回，不能跟随的已经在
// checkRedirect 里变成错误了。到闸门的只剩「3xx 但没有可用的 Location」，
// 探针实测这种响应会无错误地原样返回。它不是一个能解析的上游错误响应，
// 交给 DecodeError 只会把一个通常为空的体归成笼统上游错误。
const unfollowableRedirect = "upstream returned a redirect that cannot be followed"
