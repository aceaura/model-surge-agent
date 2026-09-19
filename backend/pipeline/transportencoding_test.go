package pipeline_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/capture"
	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/relaymock"
)

// noAutoGzipClient 是一个不让标准库代劳解压的客户端。
//
// 这一步是这组用例成立的前提，探针实测过：默认的 Transport 会自己加
// Accept-Encoding: gzip，收到压缩响应后透明解压并把 Content-Encoding 从
// 响应头里删掉。用默认客户端的话，测试里压缩的上游会被标准库解好，
// decodeBody 拿到的永远是没有 Content-Encoding 的响应体——整组断言全靠
// 标准库通过，我们这一层写成什么样都测不出来。
//
// DisableCompression 正是线上那个形态：只要有人显式设过 Accept-Encoding
// （见 outboundheaders.go 的注释），标准库就同样不再解压。
func noAutoGzipClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DisableCompression = true
	return &http.Client{Transport: tr}
}

// writeGzipStream 回一份 gzip 压缩的 SSE。
//
// 手写 Content-Encoding 而不是靠中间件：这条用例要的正是标准库没有透明解压
// 的那个形态。httptest 的 server 不会自己压。
func writeGzipStream(w http.ResponseWriter, raw string) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(raw))
	_ = zw.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Content-Encoding", "gzip")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// 上游回 gzip 压缩的 SSE 时必须照常出内容。
//
// 不解压的症状是「HTTP 200 却一帧都没解出来」，那条路径判可重试，
// 于是三个目标全走一遍才失败——而每个目标都会遇到同一件事。
func TestGzipUpstreamStreamStillDelivers(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeGzipStream(w, okStream)
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})
	f.p.HTTP = noAutoGzipClient()

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "hello") {
		t.Errorf("压缩的上游流没解出内容：%q", w.Body)
	}
	if up.calls() != 1 {
		t.Errorf("上游被打了 %d 次：压缩体没解，落到了可重试路径", up.calls())
	}
	rec := f.col.record(t)
	if rec.Outcome != "normal" {
		t.Errorf("outcome = %q，error = %q", rec.Outcome, rec.ErrorMessage)
	}
}

// 上游忽略 stream:true 回一整份压缩 JSON 时走的是另一条 body 读取路径，
// 同样要解压。
//
// 关键在于 Content-Type 不是 text/event-stream：本服务对上游一律请求流式，
// 所以「非流式客户端」并不足以走到整份响应那条分支，决定分支的是上游回的
// 外形。这条用例的上一版声明了 event-stream，于是它走的仍是 SSE 路径，
// adoptWholeResponse 的解压根本没被覆盖。
func TestGzipUpstreamWholeResponseStillDelivers(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant",` +
			`"model":"kimi-k3-256k","stop_reason":"end_turn",` +
			`"content":[{"type":"text","text":"hello"}],` +
			`"usage":{"input_tokens":100,"output_tokens":7}}`))
		_ = zw.Close()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(buf.Bytes())
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})
	f.p.HTTP = noAutoGzipClient()

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, false))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "hello") {
		t.Errorf("整份响应路径没解出压缩内容：%q", w.Body)
	}
	rec := f.col.record(t)
	// 前提确认：这条必须真的走到了整份响应那条分支。走 SSE 路径的话
	// 这条说明不会出现，而那意味着断言测的是另一段代码。
	found := false
	for _, n := range rec.Lossy {
		if strings.Contains(n, "whole response") {
			found = true
		}
	}
	if !found {
		t.Fatalf("没走到整份响应那条分支，这条用例测的是 SSE 路径：%v", rec.Lossy)
	}
}

// 捕获里留的必须是解压后的可读字节。
//
// 存压缩字节等于把「上游到底回了什么」这条最后的线索变成一段谁也看不懂的
// 二进制，而排查编码类问题时恰好只有它可看。
func TestCaptureHoldsDecompressedUpstreamBytes(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeGzipStream(w, okStream)
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})
	f.p.HTTP = noAutoGzipClient()

	st := capture.New(capture.Options{Mode: capture.ModeAll})
	c := call(t, true)
	c.Capture = st.Begin(c.RequestID)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}

	got := string(snapshotOf(t, st, c.RequestID).Bodies[capture.UpstreamResponse].Bytes)
	if !strings.Contains(got, "message_start") {
		t.Errorf("捕获里不是可读字节：%q", got)
	}
}

// 上游的错误体也压缩时必须解出来：限流与配额耗尽正是靠这个体区分的，
// 解不出会归成一个笼统的上游错误。
func TestGzipUpstreamErrorBodyIsDecoded(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write([]byte(
			`{"type":"error","error":{"type":"rate_limit_error","message":"slow down please"}}`))
		_ = zw.Close()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write(buf.Bytes())
	}}
	url := up.start(t)
	// 限流可重试，所以要给满重试预算的目标数：少给的话最终错误会是
	// 「调度层没目标了」，把要断言的那条归因盖掉。
	f := newFixture(t,
		relaymock.Step{Target: target(url, "kimi-1/k3")},
		relaymock.Step{Target: target(url, "kimi-1/k3")},
		relaymock.Step{Target: target(url, "kimi-1/k3")},
	)
	f.p.HTTP = noAutoGzipClient()

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, false))

	if w.Code == http.StatusOK {
		t.Fatalf("这条本该失败：%s", w.Body)
	}
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d，想要 429：归因没走到限流那条", w.Code)
	}
	rec := f.col.record(t)
	if !strings.Contains(rec.ErrorMessage, "slow down please") {
		t.Errorf("压缩的错误体没解出来，归因丢了上游的说法：%q", rec.ErrorMessage)
	}
}

// br 上游必须明确报错，而不是把压缩字节喂进切帧器。
func TestBrotliUpstreamFailsExplicitly(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "br")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("\x1b\x0e\x00\x00compressed-ish"))
	}}
	url := up.start(t)
	// 归成 ErrUpstream 后仍可重试，所以给满预算，否则最终错误会变成
	// 「调度层没目标了」。
	f := newFixture(t,
		relaymock.Step{Target: target(url, "kimi-1/k3")},
		relaymock.Step{Target: target(url, "kimi-1/k3")},
		relaymock.Step{Target: target(url, "kimi-1/k3")},
	)
	f.p.HTTP = noAutoGzipClient()

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if w.Code == http.StatusOK {
		t.Fatalf("这条本该失败：%s", w.Body)
	}
	rec := f.col.record(t)
	if !strings.Contains(rec.ErrorMessage, "br") {
		t.Errorf("错误没点名编码，从症状反推要花很久：%q", rec.ErrorMessage)
	}
}

// 调度层下发 Accept-Encoding 时必须丢掉它并留说明。
//
// 这是本轮最核心的一条：配了这个头之后请求正常发出、上游正常回 200，
// 只是标准库不再透明解压，症状远离原因。
func TestDispatchSuppliedAcceptEncodingIsDroppedWithNote(t *testing.T) {
	var sawAcceptEncoding string
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	url := up.start(t)

	tgt := target(url, "kimi-1/k3")
	// 刻意用 br 而不是 gzip：标准库自己会加一个 gzip，配成 gzip 的话
	// 「拦住了」与「没拦住」在线上看到的头一模一样，断言等于没做。
	tgt.Headers["Accept-Encoding"] = "br"
	f := newFixture(t, relaymock.Step{Target: tgt})

	// 上游侧核实这个头真的没发出去：只查 lossy 说明的话，
	// 「记了说明但还是写了头」这个形态测不出来。
	f.p.HTTP = &http.Client{Transport: headerSpy{
		inner: http.DefaultTransport,
		seen:  func(h http.Header) { sawAcceptEncoding = h.Get("Accept-Encoding") },
	}}

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if strings.Contains(sawAcceptEncoding, "br") {
		t.Errorf("配置里的 Accept-Encoding 被写进了出站请求：%q", sawAcceptEncoding)
	}
	if !strings.Contains(w.Body.String(), "hello") {
		t.Errorf("流没解出内容：%q", w.Body)
	}

	rec := f.col.record(t)
	found := false
	for _, n := range rec.Lossy {
		if strings.Contains(n, "Accept-Encoding") {
			found = true
		}
	}
	if !found {
		t.Errorf("丢了 Accept-Encoding 却没留说明，运维查不出为什么：%v", rec.Lossy)
	}
}

// 端点定义里的额外头同样过保护集合。
//
// 现有四个协议的 Endpoint 都不返回受保护的头，所以这条缺口在端到端跑不出来，
// 只能注册一个返回它的测试协议。这个层次真实存在：新接一家上游要求某个
// 固定头时，加的就是这里。
func TestEndpointExtraHeadersAreAlsoProtected(t *testing.T) {
	codec.RegisterOutbound(newBadExtraCodec(t))
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	url := up.start(t)

	tgt := target(url, "kimi-1/k3")
	tgt.Protocol = badExtraProtocol
	f := newFixture(t, relaymock.Step{Target: tgt})

	var sawAcceptEncoding string
	f.p.HTTP = &http.Client{Transport: headerSpy{
		inner: http.DefaultTransport,
		seen:  func(h http.Header) { sawAcceptEncoding = h.Get("Accept-Encoding") },
	}}

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if strings.Contains(sawAcceptEncoding, "br") {
		t.Errorf("端点 extra 里的 Accept-Encoding 被写进了出站请求：%q", sawAcceptEncoding)
	}
	rec := f.col.record(t)
	found := false
	for _, n := range rec.Lossy {
		if strings.Contains(n, "Accept-Encoding") {
			found = true
		}
	}
	if !found {
		t.Errorf("端点 extra 被丢了却没留说明：%v", rec.Lossy)
	}
}

// badExtraProtocol 是只在测试里注册的协议名，它的 Endpoint 返回一个受保护的头。
const badExtraProtocol = "anthropic_bad_extra_headers"

// badExtraCodec 复用 anthropic 的全部行为，只改 Name 与 Endpoint。
//
// 嵌入注册表里已有的那份而不是从零实现：OutboundCodec 有七个方法，
// 手写一份桩会让这条用例在接口变动时跟着坏，而它要测的与那些方法无关。
type badExtraCodec struct{ codec.OutboundCodec }

func newBadExtraCodec(t *testing.T) badExtraCodec {
	t.Helper()
	inner, ok := codec.Outbound(codec.ProtocolAnthropic)
	if !ok {
		t.Fatal("anthropic 出站 codec 没注册")
	}
	return badExtraCodec{OutboundCodec: inner}
}

func (badExtraCodec) Name() string { return badExtraProtocol }

func (c badExtraCodec) Endpoint(baseURL, nativeModel string, stream bool) (string, map[string]string) {
	url, _ := c.OutboundCodec.Endpoint(baseURL, nativeModel, stream)
	return url, map[string]string{"Accept-Encoding": "br"}
}

// headerSpy 把发出去的请求头交给回调后照常转发。
type headerSpy struct {
	inner http.RoundTripper
	seen  func(http.Header)
}

func (s headerSpy) RoundTrip(r *http.Request) (*http.Response, error) {
	s.seen(r.Header)
	return s.inner.RoundTrip(r)
}

// Host 与 Content-Length 这类头被丢掉也要留说明，且请求照常成功。
func TestOtherProtectedHeadersFromDispatchAreDropped(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	url := up.start(t)

	tgt := target(url, "kimi-1/k3")
	tgt.Headers["Content-Length"] = "0"
	tgt.Headers["Connection"] = "close"
	f := newFixture(t, relaymock.Step{Target: tgt})

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	rec := f.col.record(t)
	var cl, conn bool
	for _, n := range rec.Lossy {
		if strings.Contains(n, "Content-Length") {
			cl = true
		}
		if strings.Contains(n, "Connection") {
			conn = true
		}
	}
	if !cl || !conn {
		t.Errorf("被丢的头没都留说明：%v", rec.Lossy)
	}
	// 凭据头不受影响：与受保护头在同一个 map 里，一起过的那条路径
	// 不能把它也拦下来。
	if !strings.Contains(w.Body.String(), "hello") {
		t.Errorf("请求没成功，凭据头可能被一起拦了：%q", w.Body)
	}
}
