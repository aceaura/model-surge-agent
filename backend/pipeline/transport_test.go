package pipeline_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/config"
	"github.com/aceaura/model-surge-agent/backend/pipeline"
	"github.com/aceaura/model-surge-agent/backend/relaymock"
)

// 本文件守的是出站连接层：连接复用、响应头等待的时限、以及这两者不能
// 反过来伤到正常的长流。
//
// 这一层的故障都是无症状的：不复用连接只是慢，上游永不回头只是卡住，
// 两者都不报错，所以只能靠断言连接层的实际行为来守。

func transportOf(t *testing.T, c *http.Client) *http.Transport {
	t.Helper()
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport 类型 = %T，连接层参数无处可设", c.Transport)
	}
	return tr
}

// 零值取内置默认，且 PerHost 必须远大于标准库的 2。
func TestDefaultsRaisePerHostIdleConns(t *testing.T) {
	tr := transportOf(t, pipeline.NewHTTPClient(pipeline.TransportOptions{}))

	if tr.MaxIdleConnsPerHost <= http.DefaultMaxIdleConnsPerHost {
		t.Errorf("MaxIdleConnsPerHost = %d，不显式抬高等于没设，"+
			"标准库默认只留 %d 条空闲连接",
			tr.MaxIdleConnsPerHost, http.DefaultMaxIdleConnsPerHost)
	}
	if tr.MaxIdleConns < tr.MaxIdleConnsPerHost {
		t.Errorf("MaxIdleConns = %d < PerHost = %d，总量卡死会让 PerHost 白设",
			tr.MaxIdleConns, tr.MaxIdleConnsPerHost)
	}
	if tr.IdleConnTimeout <= 0 {
		t.Errorf("IdleConnTimeout = %v，空闲连接永不回收会攒住已经死掉的连接",
			tr.IdleConnTimeout)
	}
	if tr.ResponseHeaderTimeout <= 0 {
		t.Errorf("ResponseHeaderTimeout = %v，上游接受连接却永不回头时会无限等待",
			tr.ResponseHeaderTimeout)
	}
}

// 响应头等待的默认值必须宽于首帧超时的默认值，否则会把本该由首帧超时
// 报出的故障错报成连接层问题，而后者不换目标的语义与前者不同。
func TestDefaultResponseHeaderTimeoutExceedsFirstTokenDefault(t *testing.T) {
	// 只填必需项，首帧超时留空取默认——本例比的就是两个默认值的量级关系。
	t.Setenv("MSA_FIRST_TOKEN_TIMEOUT", "")
	t.Setenv("MSA_PG_DSN", "postgres://msa:msa@127.0.0.1:5432/msa")
	t.Setenv("MSA_RELAY_BASE_URL", "http://127.0.0.1:1")
	t.Setenv("MSA_RELAY_DISPATCH_KEY", "dispatch-key")
	t.Setenv("MSA_ADMIN_KEY", "admin-key")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	tr := transportOf(t, pipeline.NewHTTPClient(pipeline.TransportOptions{}))
	if tr.ResponseHeaderTimeout < cfg.FirstTokenTimeout {
		t.Errorf("ResponseHeaderTimeout = %v < FirstTokenTimeout 默认 %v，"+
			"连接层会先掐断，首帧超时那条判定永远走不到",
			tr.ResponseHeaderTimeout, cfg.FirstTokenTimeout)
	}
}

// 显式值优先于默认。
func TestExplicitOptionsWin(t *testing.T) {
	tr := transportOf(t, pipeline.NewHTTPClient(pipeline.TransportOptions{
		MaxIdleConns:          7,
		MaxIdleConnsPerHost:   5,
		IdleConnTimeout:       3 * time.Second,
		ResponseHeaderTimeout: 4 * time.Second,
	}))
	if tr.MaxIdleConns != 7 || tr.MaxIdleConnsPerHost != 5 {
		t.Errorf("空闲连接数 = %d/%d，want 7/5", tr.MaxIdleConns, tr.MaxIdleConnsPerHost)
	}
	if tr.IdleConnTimeout != 3*time.Second {
		t.Errorf("IdleConnTimeout = %v, want 3s", tr.IdleConnTimeout)
	}
	if tr.ResponseHeaderTimeout != 4*time.Second {
		t.Errorf("ResponseHeaderTimeout = %v, want 4s", tr.ResponseHeaderTimeout)
	}
}

// 负值是「显式不设限」，零值不是——零值是环境变量没配的自然形态，
// 让它表示不设限会让忘配的部署静默回到无限等待。
func TestNegativeMeansUnlimitedAndZeroMeansDefault(t *testing.T) {
	neg := transportOf(t, pipeline.NewHTTPClient(pipeline.TransportOptions{
		IdleConnTimeout:       -1,
		ResponseHeaderTimeout: -1,
	}))
	if neg.ResponseHeaderTimeout != 0 || neg.IdleConnTimeout != 0 {
		t.Errorf("负值未映射成不设限：header=%v idle=%v",
			neg.ResponseHeaderTimeout, neg.IdleConnTimeout)
	}
	zero := transportOf(t, pipeline.NewHTTPClient(pipeline.TransportOptions{}))
	if zero.ResponseHeaderTimeout == 0 {
		t.Error("零值被当成了不设限，忘配环境变量的部署会退回无限等待")
	}
}

// 从 DefaultTransport 克隆的那些设置必须还在，尤其是代理：
// 丢了代理，需要走代理出网的部署会「上游不可达」，且看不出根因在这里。
func TestClonedTransportKeepsDefaultDialSettings(t *testing.T) {
	tr := transportOf(t, pipeline.NewHTTPClient(pipeline.TransportOptions{}))
	if tr.Proxy == nil {
		t.Error("Proxy 为 nil，走代理出网的部署会连不上上游")
	}
	if tr.TLSHandshakeTimeout == 0 {
		t.Error("TLSHandshakeTimeout 为零，握手卡住时会无限等待")
	}
	if !tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 被丢掉了")
	}
}

// 不能改 DefaultTransport 本体：那会污染 relayclient 与任何用默认客户端的代码。
func TestDefaultTransportNotMutated(t *testing.T) {
	def, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Skip("DefaultTransport 不是 *http.Transport")
	}
	before := def.MaxIdleConnsPerHost
	beforeHeader := def.ResponseHeaderTimeout

	pipeline.NewHTTPClient(pipeline.TransportOptions{
		MaxIdleConnsPerHost:   99,
		ResponseHeaderTimeout: 5 * time.Second,
	})

	if def.MaxIdleConnsPerHost != before || def.ResponseHeaderTimeout != beforeHeader {
		t.Errorf("DefaultTransport 被改了：PerHost %d→%d, header %v→%v，"+
			"这是进程级的隐蔽副作用",
			before, def.MaxIdleConnsPerHost, beforeHeader, def.ResponseHeaderTimeout)
	}
}

// 不设 Client.Timeout：它覆盖到读完整个响应体，SSE 会被从中间掐断。
func TestNoWholeRequestTimeout(t *testing.T) {
	c := pipeline.NewHTTPClient(pipeline.TransportOptions{})
	if c.Timeout != 0 {
		t.Errorf("Client.Timeout = %v，它覆盖读正文，长流会被从中间掐断", c.Timeout)
	}
}

// 连接真的被复用：只断言字段等于断言配置写对了，不等于连接层真的省了握手。
func TestConnectionsAreReusedAcrossRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := pipeline.NewHTTPClient(pipeline.TransportOptions{})
	var newConns, reused atomic.Int64
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			if info.Reused {
				reused.Add(1)
				return
			}
			newConns.Add(1)
		},
	}

	const rounds = 8
	for i := 0; i < rounds; i++ {
		req, err := http.NewRequestWithContext(
			httptrace.WithClientTrace(context.Background(), trace),
			http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	if newConns.Load() != 1 {
		t.Errorf("新建连接 %d 条（复用 %d 次），串行请求应当只握手一次",
			newConns.Load(), reused.Load())
	}
}

// 并发超过标准库默认 PerHost=2 时仍能复用：这是抬高 PerHost 的实际效果，
// 不抬高的话第二轮会重新握手。
func TestConcurrentRequestsReuseBeyondStdlibPerHostDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := pipeline.NewHTTPClient(pipeline.TransportOptions{})
	const parallel = 8
	var newConns, reused atomic.Int64
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			if info.Reused {
				reused.Add(1)
				return
			}
			newConns.Add(1)
		},
	}

	burst := func() {
		var wg sync.WaitGroup
		for i := 0; i < parallel; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req, err := http.NewRequestWithContext(
					httptrace.WithClientTrace(context.Background(), trace),
					http.MethodGet, srv.URL, nil)
				if err != nil {
					return
				}
				resp, err := c.Do(req)
				if err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}()
		}
		wg.Wait()
	}

	burst()
	first := newConns.Load()
	burst()

	// 第二轮的并发量与第一轮相同，空闲池装得下时应当一条新连接都不建。
	if got := newConns.Load() - first; got != 0 {
		t.Errorf("第二轮新建了 %d 条连接（首轮 %d，累计复用 %d），"+
			"PerHost 放不下并发量时每轮都要重新握手", got, first, reused.Load())
	}
}

// 上游接受连接却永不发响应头：必须在响应头等待时限内返回错误，
// 而不是永久占住一个 goroutine 和它持有的全部缓冲。
func TestResponseHeaderTimeoutCutsSilentUpstream(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()

	c := pipeline.NewHTTPClient(pipeline.TransportOptions{
		ResponseHeaderTimeout: 150 * time.Millisecond,
	})
	start := time.Now()
	resp, err := c.Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("上游从未发响应头，却没报错——请求会永久卡住")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("等了 %v 才报错，响应头等待时限没生效", elapsed)
	}
}

// 响应头到了之后慢慢发正文：不能被响应头时限掐断，这是长流的正常形态。
func TestSlowBodyAfterHeadersIsNotCut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		for i := 0; i < 4; i++ {
			time.Sleep(120 * time.Millisecond)
			_, _ = w.Write([]byte("data: tick\n\n"))
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
	}))
	defer srv.Close()

	// 时限远小于发完正文所需的时间：头已到达就不该再受它约束。
	c := pipeline.NewHTTPClient(pipeline.TransportOptions{
		ResponseHeaderTimeout: 150 * time.Millisecond,
	})
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读正文被掐断了：%v，响应头时限不该管到正文", err)
	}
	if n := len(body); n == 0 {
		t.Error("正文为空，慢正文被截断了")
	}
}

// deadlineWriter 记下数据面推过的写 deadline。
// 自己实现 SetWriteDeadline，http.ResponseController 会优先用它。
type deadlineWriter struct {
	http.ResponseWriter
	mu   sync.Mutex
	list []time.Time
}

func (w *deadlineWriter) SetWriteDeadline(t time.Time) error {
	w.mu.Lock()
	w.list = append(w.list, t)
	w.mu.Unlock()
	return nil
}

func (w *deadlineWriter) deadlines() []time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]time.Time(nil), w.list...)
}

// 流式响应每次写之前都要把写 deadline 往后推。
//
// 不推的后果是慢客户端能无限占住一条上游连接：w.Write 是同步调用、
// 不在任何 select 里，空闲超时的计时器与客户端取消都观察不到它阻塞。
//
// 逐帧推进而非一次性总时限：后者对跑几分钟的 SSE 必然撞上。
func TestStreamingPushesWriteDeadlinePerFrame(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, `event: message_start
data: {"type":"message_start","message":{"id":"msg_1"}}

`)
		// 两批之间留出可分辨的时间差，用来验证 deadline 确实在推进。
		time.Sleep(60 * time.Millisecond)
		_, _ = w.Write([]byte(`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}

event: message_stop
data: {"type":"message_stop"}

`))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})

	var dw *deadlineWriter
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dw = &deadlineWriter{ResponseWriter: w}
		f.p.Serve(r.Context(), dw, call(t, true))
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	got := dw.deadlines()
	if len(got) < 2 {
		t.Fatalf("写 deadline 只推了 %d 次，逐帧推进才能既挡住慢客户端"+
			"又不掐断长流", len(got))
	}
	if !got[len(got)-1].After(got[0]) {
		t.Error("deadline 没有随写推进，等于一次性总时限，长流会被掐断")
	}
	if d := time.Until(got[0]); d <= 0 {
		t.Errorf("首个 deadline 已经过期（%v），每次写都会立刻失败", d)
	}
}
