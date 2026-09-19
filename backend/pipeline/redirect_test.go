package pipeline_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/relaymock"
)

// redirectPair 起两台服务器：src 发重定向，dest 收。
//
// crossHost 为真时 Location 指向 dest 的 localhost 形态，而 src 用的是
// 127.0.0.1——两个名字指同一个监听口，但标准库按名字判 host，探针实测这就足以
// 让它认为换了 host。这比改 /etc/hosts 可靠，也不需要真的有第二台机器。
func redirectPair(t *testing.T, code int, crossHost bool) (srcURL string, gotHeaders func() http.Header, destHits func() int) {
	t.Helper()
	var last http.Header
	hits := 0

	// 同 host 的形态必须是同一台服务器的两个路径：两台 httptest 服务器一定
	// 占不同端口，而端口不同就是跨 host，那样「同 host」这一支根本造不出来。
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/moved") {
			last = r.Header.Clone()
			hits++
			writeStream(w, okStream)
			return
		}
		dest := srv.URL
		if crossHost {
			// 同一个监听口的另一个名字：标准库按名字判 host，
			// 探针实测这就足以让它认为换了 host。
			dest = strings.Replace(dest, "127.0.0.1", "localhost", 1)
		}
		http.Redirect(w, r, dest+"/moved?key=sk-inquery", code)
	}))
	t.Cleanup(srv.Close)

	return srv.URL, func() http.Header { return last }, func() int { return hits }
}

// 这一条是本轮的核心：凭据绝不能到第二个 host。
//
// 必须端到端而不是只调策略函数——它测的是标准库拿着我们改过的 req
// 随后到底发了什么。307 而不是 302：302 会先被方法判据拦住，
// 那样这条用例就变成了在测另一条规则。
func TestCrossHostRedirectNeverDeliversCredentials(t *testing.T) {
	srcURL, headers, hits := redirectPair(t, http.StatusTemporaryRedirect, true)
	f := newFixture(t,
		relaymock.Step{Target: target(srcURL, "kimi-1/k3")},
		relaymock.Step{Target: target(srcURL, "kimi-1/k3")},
		relaymock.Step{Target: target(srcURL, "kimi-1/k3")},
	)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, false))

	if w.Code == http.StatusOK {
		t.Fatalf("跨 host 重定向被跟随了：%s", w.Body)
	}
	if n := hits(); n != 0 {
		t.Fatalf("第二个 host 被打了 %d 次，本轮的结论是跨 host 一律不追", n)
	}
	// 纵深防御那一层也要钉：即使将来有人把「不追」放宽，凭据也不该跟着走。
	if h := headers(); h != nil {
		for _, name := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Api-Key", "Cookie"} {
			if got := h.Get(name); got != "" {
				t.Errorf("凭据头 %s 到了第二个 host：%q", name, got)
			}
		}
	}
	rec := f.col.record(t)
	if rec.ErrorCode == "" {
		t.Error("跨 host 重定向没在流水上留错误码")
	}
	if !strings.Contains(rec.ErrorMessage, "different host") {
		t.Errorf("归因没点出跨 host：%q", rec.ErrorMessage)
	}
	if strings.Contains(rec.ErrorMessage, "://") ||
		strings.Contains(rec.ErrorMessage, "sk-inquery") {
		t.Errorf("流水上的 message 带了重定向目标 URL：%q", rec.ErrorMessage)
	}
}

// 跨 host 时摘掉凭据这件事必须留说明，且说明要进流水的 lossy。
func TestCrossHostRedirectRecordsStrippedHeaderNote(t *testing.T) {
	srcURL, _, _ := redirectPair(t, http.StatusTemporaryRedirect, true)
	f := newFixture(t,
		relaymock.Step{Target: target(srcURL, "kimi-1/k3")},
		relaymock.Step{Target: target(srcURL, "kimi-1/k3")},
		relaymock.Step{Target: target(srcURL, "kimi-1/k3")},
	)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, false))
	if w.Code == http.StatusOK {
		t.Fatalf("这条本该失败：%s", w.Body)
	}

	rec := f.col.record(t)
	lossy := strings.Join(rec.Lossy, "|")
	if !strings.Contains(lossy, "stripped credential header") {
		t.Errorf("摘凭据没留说明，运维看不出重定向动过请求头：%q", lossy)
	}
	if !strings.Contains(lossy, "X-Api-Key") {
		t.Errorf("说明没点名被摘的头：%q", lossy)
	}
	if strings.Contains(lossy, "sk-secret-1234") {
		t.Errorf("说明里带上了凭据的值：%q", lossy)
	}
}

// 301/302/303 三个码逐个：标准库会把 POST 改写成无体 GET，
// 上游随后回的 4xx 会被归成模型失败，把健康账号推向冷却。
func TestMethodRewritingRedirectsAreRefusedEndToEnd(t *testing.T) {
	for name, code := range map[string]int{
		"301": http.StatusMovedPermanently,
		"302": http.StatusFound,
		"303": http.StatusSeeOther,
	} {
		t.Run(name, func(t *testing.T) {
			// 同 host：要测的是方法判据，跨 host 会先被 host 判据拦住。
			srcURL, _, hits := redirectPair(t, code, false)
			f := newFixture(t,
				relaymock.Step{Target: target(srcURL, "kimi-1/k3")},
				relaymock.Step{Target: target(srcURL, "kimi-1/k3")},
				relaymock.Step{Target: target(srcURL, "kimi-1/k3")},
			)

			w := httptest.NewRecorder()
			f.p.Serve(context.Background(), w, call(t, false))
			if w.Code == http.StatusOK {
				t.Fatalf("方法改写型重定向被跟随了：%s", w.Body)
			}
			if n := hits(); n != 0 {
				t.Errorf("重定向目标被打了 %d 次，而那一次是无体的 GET", n)
			}
			rec := f.col.record(t)
			if !strings.Contains(rec.ErrorMessage, "rewrites the request method") {
				t.Errorf("归因没点出方法被改写：%q", rec.ErrorMessage)
			}
			if strings.Contains(rec.ErrorMessage, "://") {
				t.Errorf("message 带了 URL：%q", rec.ErrorMessage)
			}
		})
	}
}

// 重定向类失败必须可重试：换目标有意义，这是这个目标的路由配置问题。
// 判据看尝试次数——三个 relaymock 答案被用光就说明确实换目标重试了。
func TestRedirectFailureIsRetriedAcrossTargets(t *testing.T) {
	srcURL, _, _ := redirectPair(t, http.StatusFound, false)
	f := newFixture(t,
		relaymock.Step{Target: target(srcURL, "kimi-1/k3")},
		relaymock.Step{Target: target(srcURL, "kimi-2/k3")},
		relaymock.Step{Target: target(srcURL, "ark-1/ds")},
	)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, false))
	if w.Code == http.StatusOK {
		t.Fatalf("这条本该失败：%s", w.Body)
	}
	rec := f.col.record(t)
	if rec.Attempts < 2 {
		t.Errorf("attempts = %d，重定向失败没被判可重试——"+
			"换目标对路由配置问题是有意义的", rec.Attempts)
	}
}

// 307 同 host 必须跟随成功：GetBody 存在，体能正确重放。
// 这是「不要把策略写成一律拒绝」的守卫。
func TestSameHostMethodPreservingRedirectSucceeds(t *testing.T) {
	srcURL, headers, hits := redirectPair(t, http.StatusTemporaryRedirect, false)
	f := newFixture(t,
		relaymock.Step{Target: target(srcURL, "kimi-1/k3")},
		relaymock.Step{Target: target(srcURL, "kimi-1/k3")},
		relaymock.Step{Target: target(srcURL, "kimi-1/k3")},
	)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, false))
	if w.Code != http.StatusOK {
		t.Fatalf("同 host 的 307 没跟随成功：status = %d, %s", w.Code, w.Body)
	}
	if n := hits(); n != 1 {
		t.Fatalf("重定向目标被打了 %d 次, want 1", n)
	}
	// 同 host 凭据必须还在，否则一次正常的重定向会变成 401。
	if got := headers().Get("x-api-key"); got != "sk-secret-1234" {
		t.Errorf("同 host 重定向把凭据摘了：%q", got)
	}
	if !strings.Contains(w.Body.String(), "hello") {
		t.Errorf("跟随之后的响应没解出内容：%q", w.Body)
	}
	rec := f.col.record(t)
	if lossy := strings.Join(rec.Lossy, "|"); strings.Contains(lossy, "stripped credential") {
		t.Errorf("同 host 不该产摘除说明：%q", lossy)
	}
}

// 缺 Location 的 3xx 会被标准库无错误地原样返回（探针实测）。
// 它落到状态码闸门，此前会被 DecodeError 归成笼统上游错误。
func TestRedirectWithoutLocationIsAttributedAsRedirect(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 刻意不设 Location，且体里放一段不成结构的文字：
		// 走 DecodeError 的实现会把它归成笼统上游错误。
		w.WriteHeader(http.StatusFound)
		_, _ = w.Write([]byte("moved, see somewhere"))
	}))
	t.Cleanup(up.Close)

	f := newFixture(t,
		relaymock.Step{Target: target(up.URL, "kimi-1/k3")},
		relaymock.Step{Target: target(up.URL, "kimi-1/k3")},
		relaymock.Step{Target: target(up.URL, "kimi-1/k3")},
	)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, false))
	if w.Code == http.StatusOK {
		t.Fatalf("这条本该失败：%s", w.Body)
	}
	rec := f.col.record(t)
	if !strings.Contains(rec.ErrorMessage, "redirect that cannot be followed") {
		t.Errorf("没归成重定向，排查的人会去找一个不存在的上游错误：%q",
			rec.ErrorMessage)
	}
	// rec.StatusCode 记的是回给客户端的那个码，不是上游的 3xx；
	// 上游那个码在 attempts_trail 上。
	//
	// 必须可重试：三个答案都被用掉才说明真的换目标试了。
	// 一个没有可用 Location 的 3xx 是这个目标的配置问题，换目标有意义。
	if rec.Attempts < 2 {
		t.Errorf("attempts = %d，缺 Location 的 3xx 没被判可重试", rec.Attempts)
	}
}

// 超跳数：连续同 host 重定向。文本必须是我们的，不是标准库那条带 URL 的。
func TestRedirectLoopIsCappedWithoutLeakingURL(t *testing.T) {
	var srv *httptest.Server
	var mu sync.Mutex
	hops := 0
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hops++
		mu.Unlock()
		http.Redirect(w, r, srv.URL+"/loop?key=sk-inquery", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)

	f := newFixture(t,
		relaymock.Step{Target: target(srv.URL, "kimi-1/k3")},
		relaymock.Step{Target: target(srv.URL, "kimi-1/k3")},
		relaymock.Step{Target: target(srv.URL, "kimi-1/k3")},
	)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, false))
	if w.Code == http.StatusOK {
		t.Fatalf("回环被跟随到底了：%s", w.Body)
	}
	rec := f.col.record(t)
	if !strings.Contains(rec.ErrorMessage, "redirect chain was too long") {
		t.Errorf("归因不是本层的超跳数文本：%q", rec.ErrorMessage)
	}
	if strings.Contains(rec.ErrorMessage, "sk-inquery") ||
		strings.Contains(rec.ErrorMessage, "://") {
		t.Errorf("标准库那条带 URL 的文本漏出来了：%q", rec.ErrorMessage)
	}
	// 上限必须是本层的而不是标准库的 10 跳。光看归因文本
	// 区分不了（两者都会报错），只有上游被打了几次能区分：
	// 三个目标 × （首次 + maxRedirectHops 跳）。让标准库先触发的话
	// 每个目标会多跑好几跳，而每一跳都是一次真实的上游往返。
	mu.Lock()
	got := hops
	mu.Unlock()
	if want := 3 * (1 + maxRedirectHopsForTest); got != want {
		t.Errorf("上游被打了 %d 次, want %d——"+
			"跳数上限不是本层那个", got, want)
	}
}

// maxRedirectHopsForTest 与 pipeline 内部的 maxRedirectHops 保持一致。
//
// 这条用例在外部包，拿不到未导出的常量；内部包那一侧有
// TestExportedHopLimitMatches 钉住两者相等，改一边不改另一边会被报出。
const maxRedirectHopsForTest = 3

// 重定向类失败归 upstream 而不是 transport：后者的语义是「换条连接有意义」，
// 而重定向换连接一定得到同一个结果，还会影响账号该不该冷却的归因。
func TestRedirectFailureIsNotClassifiedAsTransport(t *testing.T) {
	srcURL, _, _ := redirectPair(t, http.StatusFound, false)
	f := newFixture(t,
		relaymock.Step{Target: target(srcURL, "kimi-1/k3")},
		relaymock.Step{Target: target(srcURL, "kimi-1/k3")},
		relaymock.Step{Target: target(srcURL, "kimi-1/k3")},
	)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, false))
	if w.Code == http.StatusOK {
		t.Fatalf("这条本该失败：%s", w.Body)
	}
	rec := f.col.record(t)
	if rec.ErrorCode == "transport" {
		t.Error("重定向被归成连接层故障：换条连接会得到同一个重定向，" +
			"而这个 kind 还会影响账号冷却的归因")
	}
	if strings.Contains(rec.ErrorMessage, "connection failed") {
		t.Errorf("走了连接层那条文本：%q", rec.ErrorMessage)
	}
}

// 流式请求走同一条策略：committed 之前失败，所以客户端仍能拿到 HTTP 错误码。
func TestRedirectFailureOnStreamingRequestStillReturnsHTTPError(t *testing.T) {
	srcURL, _, _ := redirectPair(t, http.StatusFound, false)
	f := newFixture(t,
		relaymock.Step{Target: target(srcURL, "kimi-1/k3")},
		relaymock.Step{Target: target(srcURL, "kimi-1/k3")},
		relaymock.Step{Target: target(srcURL, "kimi-1/k3")},
	)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))
	if w.Code == http.StatusOK {
		t.Fatalf("重定向发生在首帧之前，状态码本该是错误码：%s", w.Body)
	}
	if strings.Contains(w.Body.String(), "event: message_start") {
		t.Errorf("已经开始写流了，而这一步本该在 committed 之前失败：%q", w.Body)
	}
}

// 说明不能跨请求串：客户端是进程级共享的，sink 必须按请求分开。
func TestRedirectNotesDoNotLeakBetweenRequests(t *testing.T) {
	crossURL, _, _ := redirectPair(t, http.StatusTemporaryRedirect, true)
	okUp := &fakeUpstream{handler: func(n int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	okURL := okUp.start(t)

	// 先跑一条会产说明的，再跑一条干净的，共用同一个 Pipeline（同一个客户端）。
	f := newFixture(t,
		relaymock.Step{Target: target(crossURL, "kimi-1/k3")},
		relaymock.Step{Target: target(crossURL, "kimi-1/k3")},
		relaymock.Step{Target: target(crossURL, "kimi-1/k3")},
	)
	w1 := httptest.NewRecorder()
	f.p.Serve(context.Background(), w1, call(t, false))
	if w1.Code == http.StatusOK {
		t.Fatalf("第一条本该失败：%s", w1.Body)
	}

	// 第二条用另一个 fixture，但刻意共用同一个 Pipeline——
	// sink 串不串取决于客户端，而客户端在 Pipeline 上。
	clean := newFixture(t, relaymock.Step{Target: target(okURL, "kimi-1/k3")})
	f.p.Dispatch = clean.p.Dispatch
	f.p.Reporter = clean.col
	f.p.Recorder = clean.col

	w2 := httptest.NewRecorder()
	f.p.Serve(context.Background(), w2, call(t, false))
	if w2.Code != http.StatusOK {
		t.Fatalf("第二条该成功：status = %d, %s", w2.Code, w2.Body)
	}

	rec := clean.col.record(t)
	if lossy := strings.Join(rec.Lossy, "|"); strings.Contains(lossy, "stripped credential") {
		t.Errorf("第一条的说明串到了第二条上：%q", lossy)
	}
}
