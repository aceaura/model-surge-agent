package pipeline_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
	"github.com/aceaura/model-surge-agent/backend/relaymock"
)

// 判据 26：总时长上限覆盖全部重试，不是每次尝试各给一份。
//
// 三个目标各自慢到单次超时，不设总上限时总耗时是单次的三倍；客户端与反代
// 看到的是那个总数，而本服务此前没有任何东西约束它。
func TestMaxRequestDurationCoversAllRetries(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		// 比单次首字超时更久：每次尝试都会超时，于是三次尝试全走一遍。
		time.Sleep(700 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)

	f := newFixture(t,
		relaymock.Step{Target: target(srv.URL, "kimi-1/k3")},
		relaymock.Step{Target: target(srv.URL, "kimi-2/k3")},
		relaymock.Step{Target: target(srv.URL, "kimi-3/k3")},
	)
	f.p.Opts.FirstTokenTimeout = 400 * time.Millisecond
	f.p.Opts.IdleTimeout = 400 * time.Millisecond
	// 放得下一次尝试，放不下三次。
	f.p.Opts.MaxRequestDuration = 600 * time.Millisecond

	w := httptest.NewRecorder()
	begin := time.Now()
	f.p.Serve(context.Background(), w, call(t, true))
	took := time.Since(begin)

	// 单次超时 400ms × 3 = 1.2s 是不设总上限的形状。留足余量，
	// 断言的是「总上限确实在起作用」而不是某个精确耗时。
	if took > time.Second {
		t.Errorf("耗时 %s，总上限 600ms 没起作用——它被当成了每次尝试各一份", took)
	}
	if w.Code == http.StatusOK {
		t.Error("全部尝试都超时却回了 200")
	}
	if got := hits.Load(); got == 3 {
		t.Errorf("上游被打了 %d 次：总上限没能截断后续重试", got)
	}
}

// 总上限为 0（默认）时不截断：既有部署没配过它，默认收紧会打断长生成。
func TestZeroMaxRequestDurationDoesNotCutTheRequest(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		time.Sleep(300 * time.Millisecond)
		writeStream(w, okStream)
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})
	f.p.Opts.MaxRequestDuration = 0

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "hello") {
		t.Errorf("正文没到客户端：\n%s", w.Body)
	}
}

// 总上限是从客户端 ctx 派生的，不是另起一个：客户端断开仍要立刻传导下去，
// 否则一个已经没人要的请求会把重试跑完，每次重试都是一份上游账单。
func TestMaxRequestDurationStillPropagatesClientCancel(t *testing.T) {
	var hits atomic.Int64
	// 睡而不是等 r.Context().Done()：等 ctx 的写法会让 httptest 的 Close
	// 一直等这个 handler，用例即使通过也挂在清理上。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(2 * time.Second)
	}))
	t.Cleanup(srv.Close)

	f := newFixture(t,
		relaymock.Step{Target: target(srv.URL, "kimi-1/k3")},
		relaymock.Step{Target: target(srv.URL, "kimi-2/k3")},
		relaymock.Step{Target: target(srv.URL, "kimi-3/k3")},
	)
	f.p.Opts.FirstTokenTimeout = 10 * time.Second
	f.p.Opts.IdleTimeout = 10 * time.Second
	f.p.Opts.MaxRequestDuration = 30 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.p.Serve(ctx, httptest.NewRecorder(), call(t, true))
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("客户端取消没有传导到上游读取：总上限另起了一个 ctx")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("上游被打了 %d 次，want 1——取消之后还在换目标重试", got)
	}
}

// 出站 codec 说这个模型名拼不出安全 URL 时，归可重试的上游错误：
// 换目标会换模型名，下一个可能是配对的。
func TestUnsafeModelNameIsARetryableUpstreamFailure(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	url := up.start(t)

	bad := target(url, "gem-1/bad")
	bad.Protocol = codec.ProtocolGemini
	bad.NativeModel = "../v1beta/models/other"

	f := newFixture(t,
		relaymock.Step{Target: bad},
		relaymock.Step{Target: target(url, "kimi-2/k3")},
	)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	// 第一个目标连请求都没发出去，第二个正常回内容。
	if up.calls() != 1 {
		t.Errorf("上游收到 %d 次请求，want 1——不安全的 URL 被发出去了", up.calls())
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	rec := f.col.record(t)
	if rec.Attempts != 2 || rec.ModelID != "kimi-2/k3" {
		t.Errorf("record = %+v，want 换目标后成功", rec)
	}
	if got := f.col.outcomes(); len(got) != 2 || got[0] != relayclient.OutcomeRetrying {
		t.Errorf("outcomes = %v，want 首次 retrying", got)
	}
}

// 错误文案不带模型名：它由调度层给，流水的 model_id 已经记了它，
// 而错误文案会回到客户端手里。
func TestUnsafeModelNameErrorDoesNotEchoTheName(t *testing.T) {
	bad := target("http://127.0.0.1:1", "gem-1/bad")
	bad.Protocol = codec.ProtocolGemini
	bad.NativeModel = "../secret-internal-model"

	f := newFixture(t, relaymock.Step{Target: bad})
	f.p.Opts.MaxAttempts = 1

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, false))

	if w.Code == http.StatusOK {
		t.Fatalf("不安全的模型名却回了 200：%s", w.Body)
	}
	if strings.Contains(w.Body.String(), "secret-internal-model") {
		t.Errorf("错误体回显了模型名：\n%s", w.Body)
	}
	// 归因必须指向「模型名放不进 URL」，而不是一句笼统的「构造请求失败」。
	// 不挡空 URL 的话标准库会在 NewRequest 那里报 `parse "": empty url`，
	// 外部表现（状态码、重试次数、上报）与挡住时完全一样，只有这句文案不同——
	// 而排查的人拿着「构造请求失败」会去查本服务的代码，原因在目标配置里。
	if !strings.Contains(w.Body.String(), "upstream URL") {
		t.Errorf("归因文案没指向 URL 拼装，排查会被带错方向：\n%s", w.Body)
	}
}
