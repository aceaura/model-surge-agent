package pipeline_test

import (
	"context"
	"encoding/json"
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

// 本文件守「本服务自己的决定不得伪装成别人的」：预算到期不是客户端取消也
// 不是上游故障，overrides 压不掉本服务算出来的值，错误终态也要带限流头。

// ---- 请求预算到期的归因（判据 1–6）----

// 判据 1：提交后预算到期，不得记成客户端取消。
//
// 记成 canceled 的后果全在运维侧：那一类刻意不计目标失败也不算异常，
// 于是一个配得过紧的预算在所有指标上都长得像用户爱按停止键。
func TestBudgetExceededAfterCommitIsNotClientCancel(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		// 先出一帧让请求 committed，然后压着不说完。
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: message_start\n" +
			`data: {"type":"message_start","message":{"id":"m1","model":"k3","usage":{"input_tokens":1}}}` +
			"\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(time.Second)
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})
	f.p.Opts.FirstTokenTimeout = 5 * time.Second
	f.p.Opts.IdleTimeout = 5 * time.Second
	f.p.Opts.MaxRequestDuration = 400 * time.Millisecond

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	rec := f.col.record(t)
	if rec.ErrorCode == "canceled" {
		t.Errorf("预算到期被记成客户端取消：%+v", rec)
	}
	if rec.ErrorCode == "" {
		t.Errorf("预算到期没有任何错误码：%+v", rec)
	}
	if rec.Outcome == relayclient.OutcomeNormal {
		t.Errorf("预算到期被记成正常结束：outcome=%s", rec.Outcome)
	}
	if !strings.Contains(rec.ErrorMessage, "budget") {
		t.Errorf("错误文案没点明预算：%q", rec.ErrorMessage)
	}
}

// 判据 2：提交前预算到期，不得归成 upstream。
//
// 归 upstream 会累计这个目标的失败计数把它推向冷却，而目标没有任何问题。
func TestBudgetExceededBeforeCommitIsNotBlamedOnTheTarget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Second)
	}))
	t.Cleanup(srv.Close)

	f := newFixture(t, relaymock.Step{Target: target(srv.URL, "kimi-1/k3")})
	f.p.Opts.MaxAttempts = 1
	f.p.Opts.FirstTokenTimeout = 5 * time.Second
	f.p.Opts.IdleTimeout = 5 * time.Second
	f.p.Opts.MaxRequestDuration = 400 * time.Millisecond

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	rec := f.col.record(t)
	if rec.ErrorCode == "upstream" {
		t.Errorf("预算到期被归成上游故障，会冷却一个健康目标：%+v", rec)
	}
	if !strings.Contains(rec.ErrorMessage, "budget") {
		t.Errorf("错误文案没点明预算：%q", rec.ErrorMessage)
	}
}

// 判据 3：预算到期不可重试——换目标也没有时间了。
func TestBudgetExceededDoesNotBurnTheRemainingAttempts(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(time.Second)
	}))
	t.Cleanup(srv.Close)

	f := newFixture(t,
		relaymock.Step{Target: target(srv.URL, "kimi-1/k3")},
		relaymock.Step{Target: target(srv.URL, "kimi-2/k3")},
		relaymock.Step{Target: target(srv.URL, "kimi-3/k3")},
	)
	f.p.Opts.FirstTokenTimeout = 5 * time.Second
	f.p.Opts.IdleTimeout = 5 * time.Second
	f.p.Opts.MaxRequestDuration = 400 * time.Millisecond

	f.p.Serve(context.Background(), httptest.NewRecorder(), call(t, true))

	if got := hits.Load(); got != 1 {
		t.Errorf("上游被打了 %d 次，want 1——预算到期后还在换目标重试", got)
	}
}

// 判据 4：真正的客户端取消仍归 canceled，且仍不算异常。
//
// 这一条是反向守卫：把预算判据写宽（比如判 ctx.Err() 而不判 Cause）
// 会让真实的客户端取消也变成预算到期。
func TestRealClientCancelStillReportsCanceled(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: message_start\n" +
			`data: {"type":"message_start","message":{"id":"m1","model":"k3","usage":{"input_tokens":1}}}` +
			"\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(time.Second)
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})
	f.p.Opts.FirstTokenTimeout = 5 * time.Second
	f.p.Opts.IdleTimeout = 5 * time.Second
	// 预算开着但远大于本用例的时长：判据必须落在 Cause 上而不是「有没有配预算」。
	f.p.Opts.MaxRequestDuration = 30 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.p.Serve(ctx, httptest.NewRecorder(), call(t, true))
	}()
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("客户端取消没有传导")
	}

	rec := f.col.record(t)
	if rec.ErrorCode != "canceled" {
		t.Errorf("error_code = %q，want canceled", rec.ErrorCode)
	}
	if rec.Outcome != relayclient.OutcomeNormal {
		t.Errorf("outcome = %s，客户端取消不该算目标的失败", rec.Outcome)
	}
}

// 判据 5：不配预算（默认 0）时行为不变——客户端取消仍是 canceled，
// 且没有任何东西会把它认成预算到期。
func TestWithoutBudgetNothingIsAttributedToIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(600 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)

	f := newFixture(t, relaymock.Step{Target: target(srv.URL, "kimi-1/k3")})
	f.p.Opts.MaxAttempts = 1
	f.p.Opts.FirstTokenTimeout = 200 * time.Millisecond
	f.p.Opts.IdleTimeout = 200 * time.Millisecond
	f.p.Opts.MaxRequestDuration = 0

	f.p.Serve(context.Background(), httptest.NewRecorder(), call(t, true))

	rec := f.col.record(t)
	if strings.Contains(rec.ErrorMessage, "budget") {
		t.Errorf("没配预算却报了预算到期：%q", rec.ErrorMessage)
	}
	// 不钉具体类别：这条路的归因由既有的超时/上游二分决定（本机实测是
	// upstream，因为上游回了 200 但没有可解的流），本判据只守「不是预算」。
	if rec.ErrorCode == "" {
		t.Errorf("没有任何错误码：%+v", rec)
	}
}

// 判据 6：预算到期时客户端拿到的是一个明确的错误，不是一个静默截断的流。
func TestBudgetExceededTellsTheClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Second)
	}))
	t.Cleanup(srv.Close)

	f := newFixture(t, relaymock.Step{Target: target(srv.URL, "kimi-1/k3")})
	f.p.Opts.MaxAttempts = 1
	f.p.Opts.FirstTokenTimeout = 5 * time.Second
	f.p.Opts.IdleTimeout = 5 * time.Second
	f.p.Opts.MaxRequestDuration = 400 * time.Millisecond

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, false))

	if w.Code == http.StatusOK {
		t.Fatalf("预算到期却回了 200：%s", w.Body)
	}
	if !strings.Contains(w.Body.String(), "budget") {
		t.Errorf("客户端看不出是预算到期：\n%s", w.Body)
	}
}

// ---- overrides 保留键的端到端（判据 15–16）----

// 判据 15：目标配置里的 model/stream 覆盖到不了上游。
func TestReservedOverridesNeverReachUpstream(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	url := up.start(t)

	tg := target(url, "kimi-1/k3")
	tg.Overrides = json.RawMessage(`{"model":"ghost-model","stream":false,"temperature":0.1}`)
	f := newFixture(t, relaymock.Step{Target: tg})

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	body := up.body(t, 0)
	if body["model"] != "kimi-k3-256k" {
		t.Errorf("上游收到的 model = %v，want native model", body["model"])
	}
	if body["stream"] != true {
		t.Errorf("上游收到的 stream = %v，want true", body["stream"])
	}
	// 正当的覆盖照常生效。
	if body["temperature"] == nil {
		t.Errorf("正当的覆盖被一起丢了：%v", body)
	}
}

// 判据 16：跳过的说明进流水——症状不在这次请求上，这是运维唯一的线索。
func TestReservedOverrideIsRecordedAsLossy(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	url := up.start(t)

	tg := target(url, "kimi-1/k3")
	tg.Overrides = json.RawMessage(`{"stream":false}`)
	f := newFixture(t, relaymock.Step{Target: tg})

	f.p.Serve(context.Background(), httptest.NewRecorder(), call(t, true))

	rec := f.col.record(t)
	var found bool
	for _, n := range rec.Lossy {
		if strings.Contains(n, "stream") && strings.Contains(n, "override") {
			found = true
		}
	}
	if !found {
		t.Errorf("流水里没有跳过说明：%#v", rec.Lossy)
	}
}

// ---- 错误终态的限流头（判据 17–20）----

// 判据 17：429 终态带上限流头族。
//
// 最需要这族头的那一刻恰好是它们此前缺席的那一刻：客户端靠剩余量自适应
// 节流，拿不到就只能靠 Retry-After 硬等，而上游沉默时那个头也不发。
func TestRateLimitHeadersReachTheClientOnFailure(t *testing.T) {
	url := rateLimitedUpstream(t, map[string]string{
		"Retry-After":                       "30",
		"Anthropic-Ratelimit-Unified-Reset": "1758300000",
		"X-Ratelimit-Remaining-Tokens":      "0",
		"Set-Cookie":                        "session=leaked",
		"X-Request-Id":                      "upstream-only",
	})
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})
	f.p.Opts.MaxAttempts = 1

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	h := w.Result().Header
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d", w.Code)
	}
	if got := h.Get("Anthropic-Ratelimit-Unified-Reset"); got != "1758300000" {
		t.Errorf("reset 头 = %q，429 时客户端拿不到剩余量就只能硬等", got)
	}
	if got := h.Get("X-Ratelimit-Remaining-Tokens"); got != "0" {
		t.Errorf("remaining 头 = %q", got)
	}
	// 判据 19：白名单之外的不外泄。
	if got := h.Get("Set-Cookie"); got != "" {
		t.Errorf("Set-Cookie 外泄：%q", got)
	}
	if got := h.Get("X-Request-Id"); got == "upstream-only" {
		t.Errorf("把上游的 request id 当成了本服务的：%q", got)
	}
	// 判据 20：厂商侧关联键改名回传。错误终态上它最有价值——报障时
	// 要对的正是失败那一次的上游日志。
	if got := h.Get(pipeline.UpstreamRequestIDHeader); got != "upstream-only" {
		t.Errorf("上游 request id 没按 %q 回传：%q", pipeline.UpstreamRequestIDHeader, got)
	}
}

// 判据 18：Retry-After 只有一个值，且来自本服务现算的那一处。
func TestRetryAfterIsNotDuplicatedOnFailure(t *testing.T) {
	url := rateLimitedUpstream(t, map[string]string{
		"Retry-After":                  "30",
		"X-Ratelimit-Remaining-Tokens": "0",
	})
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})
	f.p.Opts.MaxAttempts = 1

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if vs := w.Result().Header["Retry-After"]; len(vs) != 1 {
		t.Errorf("Retry-After = %v，want 恰好一个值", vs)
	}
}

// 判据 20：重试后失败时，头来自最终那次尝试。
func TestForwardedHeadersComeFromTheLastAttempt(t *testing.T) {
	first := rateLimitedUpstream(t, map[string]string{
		"X-Ratelimit-Remaining-Tokens": "111",
	})
	last := rateLimitedUpstream(t, map[string]string{
		"X-Ratelimit-Remaining-Tokens": "222",
	})
	f := newFixture(t,
		relaymock.Step{Target: target(first, "kimi-1/k3")},
		relaymock.Step{Target: target(last, "kimi-2/k3")},
	)
	f.p.Opts.MaxAttempts = 2

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if got := w.Result().Header.Get("X-Ratelimit-Remaining-Tokens"); got != "222" {
		t.Errorf("remaining = %q，want 222（最后那次尝试）", got)
	}
}
