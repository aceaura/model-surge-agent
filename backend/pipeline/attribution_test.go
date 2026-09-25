package pipeline_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/pipeline"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
	"github.com/aceaura/model-surge-agent/backend/relaymock"
)

// 本文件守上游侧失败的归因走到端到端：请求已发出的失败不静默重发、
// 目标不可达计入目标失败、响应体排空后连接可复用、拦截页归因、限流头透传。

// ---- 幂等边界（判据 1–8）----

// 请求已完整交给上游之后失败，不得换目标重发。
//
// 上游可能已经生成完并计了费，重发就是第二份账单；带副作用的工具调用
// 会被执行第二次。而运维在对账时看到上游账单是本地流水的数倍，且无法
// 定位是哪些请求重复——本地只记了最后那次。
func TestResponseHeaderTimeoutDoesNotSwitchTargets(t *testing.T) {
	var hits atomic.Int64
	// 收下请求然后一直不回响应头：正是 ResponseHeaderTimeout 约束的那一刻。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(900 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)

	f := newFixture(t,
		relaymock.Step{Target: target(srv.URL, "kimi-1/k3")},
		relaymock.Step{Target: target(srv.URL, "kimi-2/k3")},
		relaymock.Step{Target: target(srv.URL, "kimi-3/k3")},
	)
	// 显式设短：默认 120s，不设的话这条用例根本碰不到响应头超时。
	f.p.HTTP = pipeline.NewHTTPClient(pipeline.TransportOptions{
		ResponseHeaderTimeout: 300 * time.Millisecond,
	})

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if got := hits.Load(); got != 1 {
		t.Errorf("上游收到 %d 次请求，want 1——请求已发出的失败被静默重发了，"+
			"每一次重发都是上游侧的第二份生成与计费", got)
	}
}

// 上游读完请求就断开：同样是已发出，同样不得重发。
func TestCloseAfterRequestDoesNotSwitchTargets(t *testing.T) {
	var hits atomic.Int64
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			hits.Add(1)
			buf := make([]byte, 8<<10)
			_, _ = c.Read(buf)
			_ = c.Close()
		}
	}()

	base := "http://" + ln.Addr().String()
	f := newFixture(t,
		relaymock.Step{Target: target(base, "kimi-1/k3")},
		relaymock.Step{Target: target(base, "kimi-2/k3")},
		relaymock.Step{Target: target(base, "kimi-3/k3")},
	)
	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if got := hits.Load(); got != 1 {
		t.Errorf("上游收到 %d 次连接，want 1——读完请求才断的失败被重发了", got)
	}
}

// 拨号失败的请求根本没发出，必须照常换目标。
//
// 这一条守的是幂等闸门没有把可重试的一整族一起挡住：挡住的症状是
// 一次连接抖动就直接失败，而那本来换个目标就好。
func TestDialFailureStillSwitchesTargets(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	good := up.start(t)
	f := newFixture(t,
		relaymock.Step{Target: target(deadBaseURL(t), "kimi-1/k3")},
		relaymock.Step{Target: target(good, "kimi-2/k3")},
	)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，拨号失败必须换目标——请求根本没发出去", w.Code)
	}
	if got := len(f.col.outcomes()); got != 2 {
		t.Errorf("上报 %d 次，want 2（一次连接故障 + 一次成功）", got)
	}
}

// 上游明确回 502：Do 返回 nil，不算已发出风险，照常换目标。
//
// 这是探针推翻设计稿的那一条：一个明确的状态码说明上游拒绝了这次请求，
// 换目标是安全的。
func TestUpstreamStatusErrorStillSwitchesTargets(t *testing.T) {
	var hits atomic.Int64
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, `{"error":{"type":"api_error","message":"bad gateway"}}`,
			http.StatusBadGateway)
	}))
	t.Cleanup(bad.Close)

	f := newFixture(t,
		relaymock.Step{Target: target(bad.URL, "kimi-1/k3")},
		relaymock.Step{Target: target(bad.URL, "kimi-2/k3")},
		relaymock.Step{Target: target(bad.URL, "kimi-3/k3")},
	)
	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if got := hits.Load(); got != 3 {
		t.Errorf("上游收到 %d 次请求，want 3——502 走状态码闸门，"+
			"不该被幂等闸门挡住", got)
	}
}

// 已发出的失败只上报一条：闸门放在上报之后会让同一次失败记两笔。
func TestSideEffectRiskReportsExactlyOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(900 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)

	f := newFixture(t,
		relaymock.Step{Target: target(srv.URL, "kimi-1/k3")},
		relaymock.Step{Target: target(srv.URL, "kimi-2/k3")},
	)
	f.p.HTTP = pipeline.NewHTTPClient(pipeline.TransportOptions{
		ResponseHeaderTimeout: 300 * time.Millisecond,
	})

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if got := f.col.outcomes(); len(got) != 1 {
		t.Errorf("上报 %d 次，want 1: %v", len(got), got)
	}
}

// 已发出的失败必须给客户端一个明确错误，而不是挂住或空响应。
//
// 客户端知道自己这个请求有没有副作用，我们不知道——把判断交给它。
func TestSideEffectRiskFailsTheRequestExplicitly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(900 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)

	f := newFixture(t, relaymock.Step{Target: target(srv.URL, "kimi-1/k3")})
	f.p.HTTP = pipeline.NewHTTPClient(pipeline.TransportOptions{
		ResponseHeaderTimeout: 300 * time.Millisecond,
	})

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if w.Code == http.StatusOK {
		t.Error("状态码 200，已发出的失败被当成成功了")
	}
	if strings.TrimSpace(w.Body.String()) == "" {
		t.Error("响应体为空——客户端只会看到「未知错误」")
	}
}

// ---- 目标不可达（判据 9、14 的端到端）----

// 域名解析不出来的目标必须计入它自己的失败，而不是「连接层故障」。
//
// 归成 transport 的后果是这个目标永不冷却，调度层持续派流量给它，
// 而运维看到「transport 占比升高但所有目标都健康」这个假结论。
func TestUnreachableTargetIsNotReportedAsTransport(t *testing.T) {
	const dead = "http://no-such-host-msa-round32.invalid"
	f := newFixture(t,
		relaymock.Step{Target: target(dead, "kimi-1/k3")},
		relaymock.Step{Target: target(dead, "kimi-2/k3")},
		relaymock.Step{Target: target(dead, "kimi-3/k3")},
	)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	outcomes := f.col.outcomes()
	if len(outcomes) == 0 {
		t.Fatal("一次都没上报")
	}
	for i, got := range outcomes {
		if got == relayclient.OutcomeTransport {
			t.Fatalf("第 %d 次上报 outcome = %q——域名不存在是这个目标的问题，"+
				"记成连接层故障会让它永不冷却", i, got)
		}
	}
}

// 不可达的错误消息里不得出现 base_url。
func TestUnreachableTargetDoesNotLeakBaseURL(t *testing.T) {
	const dead = "http://no-such-host-msa-round32.invalid/v1?key=sk-inquery"
	// 三步都给：少给的话请求会走到「mock 用尽」那条路，客户端看到的错误
	// 就不是不可达那一条，这个断言于是对着一个无关的错误文本空跑。
	f := newFixture(t,
		relaymock.Step{Target: target(dead, "kimi-1/k3")},
		relaymock.Step{Target: target(dead, "kimi-2/k3")},
		relaymock.Step{Target: target(dead, "kimi-3/k3")},
	)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))
	if !strings.Contains(w.Body.String(), "unreachable") {
		t.Fatalf("没走到不可达那条路，断言会空跑: %s", w.Body.String())
	}

	if body := w.Body.String(); strings.Contains(body, "sk-inquery") {
		t.Errorf("错误体泄露了 query 里的凭据: %s", body)
	}
	rec := f.col.record(t)
	if strings.Contains(rec.ErrorMessage, "sk-inquery") {
		t.Errorf("流水泄露了 query 里的凭据: %s", rec.ErrorMessage)
	}
}

// ---- 响应体排空（判据 15–19）----

// 超过读取上限的错误体必须排空，否则这条连接不会回到空闲池。
//
// 探针实测：200KB 错误体、连续五次同样的 429，不排空建五条新连接。
// 于是上游进 429 风暴时池配置形同未配，TLS 握手随失败率线性上涨，
// 而池饱和指标看着正常——池里没连接不是因为满，是因为没人放回来。
func TestOversizedErrorBodyKeepsConnectionReusable(t *testing.T) {
	var conns atomic.Int64
	big := strings.Repeat("x", 200<<10)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, big, http.StatusTooManyRequests)
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	f := newFixture(t,
		relaymock.Step{Target: target(srv.URL, "kimi-1/k3")},
		relaymock.Step{Target: target(srv.URL, "kimi-2/k3")},
		relaymock.Step{Target: target(srv.URL, "kimi-3/k3")},
	)
	f.p.HTTP = pipeline.NewHTTPClient(pipeline.TransportOptions{})

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if got := conns.Load(); got != 1 {
		t.Errorf("建了 %d 条连接，want 1——超限的错误体没排空，"+
			"标准库不会把这条连接放回空闲池", got)
	}
}

// 整份响应（非 SSE）超限时同样要排空。
func TestOversizedWholeResponseKeepsConnectionReusable(t *testing.T) {
	var conns atomic.Int64
	// 合法 JSON 开头但超长：走 adoptWholeResponse 那条路。
	big := `{"type":"message","content":[{"type":"text","text":"` +
		strings.Repeat("y", 200<<10) + `"}]}`
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(big))
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	f := newFixture(t,
		relaymock.Step{Target: target(srv.URL, "kimi-1/k3")},
		relaymock.Step{Target: target(srv.URL, "kimi-2/k3")},
	)
	f.p.HTTP = pipeline.NewHTTPClient(pipeline.TransportOptions{})

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if got := conns.Load(); got == 0 {
		t.Fatal("一条连接都没建，用例没跑到上游")
	} else if got > 1 {
		t.Errorf("建了 %d 条连接，整份响应那条路没排空", got)
	}
}

// 排空必须有上限：一个无限长的错误体不能把请求挂死。
func TestDrainIsBounded(t *testing.T) {
	// 远大于排空上限（1MB）。不排空上限的话这里会一直读下去。
	huge := strings.Repeat("z", 8<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, huge, http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	f := newFixture(t,
		relaymock.Step{Target: target(srv.URL, "kimi-1/k3")},
		relaymock.Step{Target: target(srv.URL, "kimi-2/k3")},
		relaymock.Step{Target: target(srv.URL, "kimi-3/k3")},
	)
	w := httptest.NewRecorder()

	// 断言读了多少字节而不是「有没有挂住」：本机上读 8MB 也只是几十毫秒，
	// 纯超时的断言对「排空无上限」这个变异完全无感。
	var read atomic.Int64
	f.p.HTTP = pipeline.NewHTTPClient(pipeline.TransportOptions{})
	f.p.HTTP.Transport = countingRoundTripper{inner: f.p.HTTP.Transport, read: &read}

	f.p.Serve(context.Background(), w, call(t, true))

	if w.Code == http.StatusOK {
		t.Error("429 被当成成功")
	}
	// 三次尝试，每次最多 64KB 错误体 + 1MB 排空，留一倍余量。
	const limit = int64(3 * ((1 << 20) + 64<<10) * 2)
	if read.Load() > limit {
		t.Errorf("读了 %d 字节，超过 %d：排空没有上限，一个超长错误体能把请求拖住",
			read.Load(), limit)
	}
}

// countingRoundTripper 统计从上游响应体上实际读了多少字节。
type countingRoundTripper struct {
	inner http.RoundTripper
	read  *atomic.Int64
}

func (c countingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := c.inner.RoundTrip(r)
	if resp != nil && resp.Body != nil {
		resp.Body = countingBody{ReadCloser: resp.Body, read: c.read}
	}
	return resp, err
}

type countingBody struct {
	io.ReadCloser
	read *atomic.Int64
}

func (c countingBody) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.read.Add(int64(n))
	return n, err
}

// 3xx 那条路同样要排空。它拿的是一个没有可用 Location 的重定向响应，
// 体通常很小但不保证——门户类设备会在 302 上回一整页说明。
func TestRedirectBranchKeepsConnectionReusable(t *testing.T) {
	var conns atomic.Int64
	page := strings.Repeat("r", 200<<10)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 不给 Location：标准库不跟随，原样回给我们，落到 3xx 那条分支。
		w.WriteHeader(http.StatusFound)
		_, _ = w.Write([]byte(page))
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	f := newFixture(t,
		relaymock.Step{Target: target(srv.URL, "kimi-1/k3")},
		relaymock.Step{Target: target(srv.URL, "kimi-2/k3")},
		relaymock.Step{Target: target(srv.URL, "kimi-3/k3")},
	)
	f.p.HTTP = pipeline.NewHTTPClient(pipeline.TransportOptions{})

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if got := conns.Load(); got == 0 {
		t.Fatal("一条连接都没建，夹具没走到")
	} else if got > 1 {
		t.Errorf("建了 %d 条连接：3xx 分支没排空响应体", got)
	}
}

// ---- 拦截页归因（判据 20、24 的端到端）----

// 200 + HTML 拦截页的错误消息必须点明 HTML 与中间设备。
//
// 不点明的话运维看到的是 `invalid character '<' ...`，
// 那句话指向「我们的解码器坏了」。
func TestHTMLInterceptPageIsAttributedToAProxy(t *testing.T) {
	page := `<!DOCTYPE html><html><head><title>403 Forbidden</title></head>` +
		`<body>Access denied by security policy</body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	}))
	t.Cleanup(srv.Close)

	// 拦截页可重试，会烧满三次 attempt：mock 步数不够会把错误换成
	// 「ran out of steps」，那样测的就不是本条判据了。
	f := newFixture(t, relaymock.Step{Target: target(srv.URL, "kimi-1/k3")})
	f.p.Opts.MaxAttempts = 1
	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	rec := f.col.record(t)
	low := strings.ToLower(rec.ErrorMessage)
	if !strings.Contains(low, "html") {
		t.Errorf("错误消息没提 HTML: %q——运维会以为是我们的解码器坏了",
			rec.ErrorMessage)
	}
	if !strings.Contains(low, "proxy") && !strings.Contains(low, "waf") {
		t.Errorf("错误消息没指向中间设备: %q", rec.ErrorMessage)
	}
}

// 拦截页仍可重试：换个目标确实可能绕过那台设备。
func TestHTMLInterceptPageIsStillRetryable(t *testing.T) {
	page := `<html><body>blocked</body></html>`
	var hits atomic.Int64
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(page))
	}))
	t.Cleanup(blocked.Close)

	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	good := up.start(t)

	f := newFixture(t,
		relaymock.Step{Target: target(blocked.URL, "kimi-1/k3")},
		relaymock.Step{Target: target(good, "kimi-2/k3")},
	)
	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if w.Code != http.StatusOK {
		t.Errorf("状态码 = %d，拦截页必须可重试", w.Code)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("被拦的目标收到 %d 次请求", got)
	}
}

// 不是 HTML 的解码失败要带上 Content-Type：一个头就能把
// 「解码器坏了」与「上游回了别的东西」分开。
func TestNonHTMLDecodeFailureNamesTheContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("\x00\x01\x02not json at all"))
	}))
	t.Cleanup(srv.Close)

	f := newFixture(t, relaymock.Step{Target: target(srv.URL, "kimi-1/k3")})
	f.p.Opts.MaxAttempts = 1
	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	rec := f.col.record(t)
	if !strings.Contains(rec.ErrorMessage, "octet-stream") {
		t.Errorf("错误消息没带 Content-Type: %q", rec.ErrorMessage)
	}
}

// ---- 限流头透传（判据 25–30 的端到端）----

// 流式路径必须把限流头传给客户端。
//
// Claude Code 一类客户端靠 unified-remaining 自适应节流，拿不到就只能
// 全速打到 429 才退避，表现为周期性硬撞限流。
func TestStreamingForwardsRateLimitHeaders(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		w.Header().Set("anthropic-ratelimit-unified-remaining", "1500")
		w.Header().Set("x-request-id", "req_vendor_1")
		w.Header().Set("Set-Cookie", "session=leak; HttpOnly")
		writeStream(w, okStream)
	}}
	f := newFixture(t, relaymock.Step{Target: target(up.start(t), "kimi-1/k3")})

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if got := w.Header().Get("Anthropic-Ratelimit-Unified-Remaining"); got != "1500" {
		t.Errorf("限流头没传给客户端: %q", got)
	}
	if got := w.Header().Get("Set-Cookie"); got != "" {
		t.Errorf("Set-Cookie 被传出去了: %q", got)
	}
	// 厂商侧关联键改名回传：客户端报障要拿它对厂商日志，原名会撞我们
	// 自己回显的 X-Request-Id。
	if got := w.Header().Get(pipeline.UpstreamRequestIDHeader); got != "req_vendor_1" {
		t.Errorf("上游 request id 没按 %q 回传: %q", pipeline.UpstreamRequestIDHeader, got)
	}
}

// 非流式路径同样要传：两条路各写自己的响应头，漏一条就是一半请求没有。
func TestNonStreamingForwardsRateLimitHeaders(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		w.Header().Set("x-ratelimit-remaining-tokens", "39000")
		w.Header().Set("x-request-id", "req_vendor_2")
		writeStream(w, okStream)
	}}
	f := newFixture(t, relaymock.Step{Target: target(up.start(t), "kimi-1/k3")})

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, false))

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", w.Code)
	}
	if got := w.Header().Get("X-Ratelimit-Remaining-Tokens"); got != "39000" {
		t.Errorf("非流式路径没传限流头: %q", got)
	}
	if got := w.Header().Get(pipeline.UpstreamRequestIDHeader); got != "req_vendor_2" {
		t.Errorf("非流式路径没传上游 request id: %q", got)
	}
}

// 流式那条路的透传必须排在 writeStreamHeaders 之前。
//
// httptest.ResponseRecorder 抓不到这一点：它的 Header() 在 WriteHeader 之后
// 仍然可写，于是「顺序写反了」在 recorder 上看起来完全正常。必须经过一个真
// net/http 服务器，让标准库把响应头真的发出去。
func TestStreamingHeaderForwardHappensBeforeWriteHeader(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		w.Header().Set("anthropic-ratelimit-unified-remaining", "1500")
		writeStream(w, okStream)
	}}
	f := newFixture(t, relaymock.Step{Target: target(up.start(t), "kimi-1/k3")})

	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.p.Serve(r.Context(), w, call(t, true))
	}))
	t.Cleanup(front.Close)

	resp, err := http.Get(front.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	_, _ = io.Copy(io.Discard, resp.Body)

	if got := resp.Header.Get("Anthropic-Ratelimit-Unified-Remaining"); got != "1500" {
		t.Errorf("限流头没真的发到线上（写在 WriteHeader 之后就是这个症状）: %q", got)
	}
}

// 超过整份响应上限时必须关掉响应体。
//
// 读到底的那一种不关也没事：标准库在 body 读到 EOF 时就把连接放回池了，
// 所以「不关体」这个变异在成功路径上是行为等价的。超限这一种不同——
// 体没读到底，不关就是把这条连接泄在那里，服务端那边一直挂着。
func TestOversizedWholeResponseClosesTheConnection(t *testing.T) {
	closed := make(chan struct{}, 4)
	// 上限是 32MiB，写得比它多一个字节就够。
	chunk := strings.Repeat("y", 1<<20)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		for i := 0; i < 33; i++ {
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateClosed {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	f := newFixture(t, relaymock.Step{Target: target(srv.URL, "kimi-1/k3")})
	f.p.Opts.MaxAttempts = 1
	// 不拆连接时这次尝试会一直挂到空闲超时。把它收紧，让「没拆」表现成
	// 一次几秒的失败而不是把整个用例挂到 go test 的总超时上。
	f.p.Opts.FirstTokenTimeout = 2 * time.Second
	f.p.Opts.IdleTimeout = 2 * time.Second
	f.p.HTTP = pipeline.NewHTTPClient(pipeline.TransportOptions{})

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if w.Code == http.StatusOK {
		t.Fatal("超限的响应不该成成功，夹具没走到那条路")
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Error("服务端没看到连接关闭：超限路径没关响应体，连接被泄掉了")
	}
}
