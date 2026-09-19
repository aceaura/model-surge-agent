package pipeline_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/relaymock"
)

// 限流终态的响应必须带 Retry-After。
//
// 客户端 SDK 的自动退避读的正是这个头。此前它一个出口都没有：上游明示的
// 到期时刻只流向调度层，客户端收到 429 后按自己的默认节奏立刻重来，
// 把一次限流变成一场风暴。
func TestRateLimitResponseCarriesRetryAfterHeader(t *testing.T) {
	url := rateLimitedUpstream(t, map[string]string{"Retry-After": "90"})
	// 三个目标全限流才走终态那一路：还能换目标时终态还没发生。
	f := newFixture(t,
		relaymock.Step{Target: target(url, "kimi-1/k3")},
		relaymock.Step{Target: target(url, "kimi-2/k3")},
		relaymock.Step{Target: target(url, "kimi-3/k3")},
	)
	rec := httptest.NewRecorder()
	f.p.Serve(context.Background(), rec, call(t, true))

	raw, ok := sentRetryAfter(rec)
	if !ok {
		t.Fatal("429 响应没带 Retry-After，客户端的自动退避读不到任何东西")
	}
	secs, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("Retry-After = %q，不是整数秒: %v", raw, err)
	}
	// 上游说 90 秒，中间有三次尝试的耗时，所以只能判区间。
	if secs <= 0 || secs > 90 {
		t.Errorf("Retry-After = %d，want (0, 90]", secs)
	}
}

// 头值必须从解析过的时刻现算，不得转发上游那个字符串。
//
// 转发的话上游就能往本服务的响应头里塞第二个头——Go 的 Header.Set 会拒绝
// 含 CRLF 的值，但这条测试守的是「不走那条路」而不是「标准库替我们兜住」。
func TestRetryAfterHeaderIsNotForwardedFromUpstream(t *testing.T) {
	// 一个语义上无效、但如果被原样转发就能看出来的值。
	url := rateLimitedUpstream(t, map[string]string{
		"Retry-After":                "99999999",
		"anthropic-ratelimit-status": "exceeded",
	})
	f := newFixture(t,
		relaymock.Step{Target: target(url, "kimi-1/k3")},
		relaymock.Step{Target: target(url, "kimi-2/k3")},
		relaymock.Step{Target: target(url, "kimi-3/k3")},
	)
	rec := httptest.NewRecorder()
	f.p.Serve(context.Background(), rec, call(t, true))

	// 99999999 秒远超 24 小时地平线，因此解析侧把它丢了，头也就不该有。
	// 原样转发的实现会在这里把这个值交给客户端，让它睡三年。
	if got, _ := sentRetryAfter(rec); got == "99999999" {
		t.Errorf("Retry-After = %q，这是上游字符串被原样转发了", got)
	}
	// 上游的其它限流头不该出现在下游响应上。
	if got := rec.Result().Header.Get("anthropic-ratelimit-status"); got != "" {
		t.Errorf("上游限流头 anthropic-ratelimit-status 漏到了客户端: %q", got)
	}
}

// 上游没给到期信息时不发这个头：编造一个会让客户端白等。
func TestNoRetryAfterHeaderWhenUpstreamIsSilent(t *testing.T) {
	url := rateLimitedUpstream(t, nil)
	f := newFixture(t,
		relaymock.Step{Target: target(url, "kimi-1/k3")},
		relaymock.Step{Target: target(url, "kimi-2/k3")},
		relaymock.Step{Target: target(url, "kimi-3/k3")},
	)
	rec := httptest.NewRecorder()
	f.p.Serve(context.Background(), rec, call(t, true))

	if got, ok := sentRetryAfter(rec); ok {
		t.Errorf("上游没说却发了 Retry-After: %q", got)
	}
}

// 成功响应不带这个头：2xx 上它没有语义。
func TestSuccessResponseHasNoRetryAfterHeader(t *testing.T) {
	up := &fakeUpstream{handler: func(n int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		// 真实上游在接近配额时会在成功响应上带限流头。
		w.Header().Set("Retry-After", "300")
		_, _ = w.Write([]byte(okStream))
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})
	rec := httptest.NewRecorder()
	f.p.Serve(context.Background(), rec, call(t, true))

	if got, ok := sentRetryAfter(rec); ok {
		t.Errorf("成功响应带了 Retry-After: %q", got)
	}
}

// 流已提交后在中途失败时不发这个头：响应头早已写出，此刻设 header
// 既不报错也不生效，而读代码的人会以为它生效了。
func TestCommittedStreamDoesNotSetRetryAfter(t *testing.T) {
	up := &fakeUpstream{handler: func(n int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// 先发一个正常帧把流提交掉，然后就此静默——空闲超时会在流中途报错。
		_, _ = w.Write([]byte("event: message_start\n" +
			`data: {"type":"message_start","message":{"id":"m","model":"k3",` +
			`"usage":{"input_tokens":1}}}` + "\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(300 * time.Millisecond)
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})
	f.p.Opts.IdleTimeout = 80 * time.Millisecond
	rec := httptest.NewRecorder()
	f.p.Serve(context.Background(), rec, call(t, true))

	if got, ok := sentRetryAfter(rec); ok {
		t.Errorf("已提交的流上设了 Retry-After: %q——那一刻设头不生效", got)
	}
}

// sentRetryAfter 读的是实际发出去的响应头。
//
// rec.Header() 是那张可写的 map：WriteHeader 之后往里设东西，标准库既不报错
// 也不生效，而 rec.Header() 照样能看见。只有 Result().Header 反映真的发出去
// 了什么。读错地方的话，一个「写头之后才设」的实现在测试里全绿，而客户端
// 一个字节都收不到。
//
// 返回 bool 而不是空串判定：Set(k, "") 会留下一个键存在、值为空串的头，
// Get 分不出它与「键不存在」，于是 != "" 形式的断言会漏过。
func sentRetryAfter(rec *httptest.ResponseRecorder) (string, bool) {
	vs, ok := rec.Result().Header["Retry-After"]
	if !ok || len(vs) == 0 {
		return "", false
	}
	return vs[0], true
}
