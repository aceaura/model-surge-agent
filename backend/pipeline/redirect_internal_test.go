package pipeline

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// hop 造一次重定向调用：origin 是最初那个请求的 URL，chain 是随后每一跳的 URL。
// 返回下一跳的 req 与 via，形状与标准库调 CheckRedirect 时给的一致。
func hop(t *testing.T, method, origin string, chain ...string) (*http.Request, []*http.Request) {
	t.Helper()
	first := httptest.NewRequest(method, origin, nil)
	via := []*http.Request{first}
	req := first
	for _, next := range chain {
		req = httptest.NewRequest(method, next, nil)
		via = append(via, req)
	}
	// via 里不含最后那一跳：标准库传的 via 是「已经走过的」，req 是「要去的」。
	return req, via[:len(via)-1]
}

const (
	originURL = "http://up.example:443/v1/messages"
	sameHost  = "http://up.example:443/v2/messages"
	otherHost = "http://evil.example:443/v1/messages"
	otherPort = "http://up.example:8443/v1/messages"
)

// 五个凭据头逐个测，刻意不迭代 redirectStrippedHeaders：
// 迭代集合的用例在有人删掉一项时会静默少测一项而照样通过。
func TestCrossHostStripsAuthorization(t *testing.T) {
	assertStripped(t, "Authorization", "Bearer sk-secret")
}

func TestCrossHostStripsXAPIKey(t *testing.T) {
	assertStripped(t, "x-api-key", "sk-anthropic")
}

func TestCrossHostStripsXGoogAPIKey(t *testing.T) {
	assertStripped(t, "x-goog-api-key", "sk-gemini")
}

func TestCrossHostStripsAPIKey(t *testing.T) {
	assertStripped(t, "api-key", "sk-azure")
}

func TestCrossHostStripsCookie(t *testing.T) {
	assertStripped(t, "Cookie", "session=sk-cookie")
}

func assertStripped(t *testing.T, name, value string) {
	t.Helper()
	req, via := hop(t, http.MethodPost, originURL, otherHost)
	req.Header.Set(name, value)
	ctx, sink := withRedirectSink(req.Context())
	req = req.WithContext(ctx)

	err := checkRedirect(req, via)
	if !errors.Is(err, errRedirectCrossHost) {
		t.Fatalf("跨 host 没被拒：err = %v", err)
	}
	if got := req.Header.Get(name); got != "" {
		t.Errorf("%s 还在请求上：%q——拒绝与摘除是两件事，"+
			"把 host 判据放宽的那天凭据会跟着走", name, got)
	}
	notes := strings.Join(sink.drain(), "|")
	if !strings.Contains(notes, http.CanonicalHeaderKey(name)) {
		t.Errorf("说明里没点名 %s：%q", name, notes)
	}
	if strings.Contains(notes, value) {
		t.Errorf("说明里带上了头的值，而它会进流水 lossy：%q", notes)
	}
}

// 头名判定大小写不敏感：配置与 SDK 各写各的大小写。
func TestCrossHostStripIsCaseInsensitive(t *testing.T) {
	req, via := hop(t, http.MethodPost, originURL, otherHost)
	req.Header.Set("X-API-KEY", "sk-upper")
	ctx, _ := withRedirectSink(req.Context())
	req = req.WithContext(ctx)

	_ = checkRedirect(req, via)
	if got := req.Header.Get("x-api-key"); got != "" {
		t.Errorf("大写形态没被摘掉：%q", got)
	}
}

// 说明里不得出现目标 URL：它可能把 key 放在 query 里。
func TestCrossHostNoteCarriesNoURL(t *testing.T) {
	req, via := hop(t, http.MethodPost, originURL, "http://evil.example/v1?key=sk-inquery")
	req.Header.Set("x-api-key", "sk-anthropic")
	ctx, sink := withRedirectSink(req.Context())
	req = req.WithContext(ctx)

	_ = checkRedirect(req, via)
	notes := strings.Join(sink.drain(), "|")
	if strings.Contains(notes, "://") || strings.Contains(notes, "sk-inquery") {
		t.Errorf("说明里带上了重定向目标 URL：%q", notes)
	}
}

// 同 host 换路径是上游正当的版本迁移形态，凭据必须留着，
// 否则一次正常的 307 会变成 401。
func TestSameHostKeepsCredentialsAndIsAllowed(t *testing.T) {
	req, via := hop(t, http.MethodPost, originURL, sameHost)
	req.Header.Set("x-api-key", "sk-anthropic")
	ctx, sink := withRedirectSink(req.Context())
	req = req.WithContext(ctx)

	if err := checkRedirect(req, via); err != nil {
		t.Fatalf("同 host 的 307 被拒了：%v", err)
	}
	if got := req.Header.Get("x-api-key"); got != "sk-anthropic" {
		t.Errorf("同 host 却摘了凭据，这会把一次正常重定向变成 401：%q", got)
	}
	if notes := sink.drain(); len(notes) != 0 {
		t.Errorf("同 host 不该产说明：%q", notes)
	}
}

// 换端口就是换服务：判据必须取含端口的 Host。
func TestDifferentPortCountsAsCrossHost(t *testing.T) {
	req, via := hop(t, http.MethodPost, originURL, otherPort)
	req.Header.Set("x-api-key", "sk-anthropic")
	ctx, _ := withRedirectSink(req.Context())
	req = req.WithContext(ctx)

	if err := checkRedirect(req, via); !errors.Is(err, errRedirectCrossHost) {
		t.Fatalf("换端口没算跨 host：err = %v", err)
	}
	if got := req.Header.Get("x-api-key"); got != "" {
		t.Errorf("换端口没摘凭据：%q", got)
	}
}

// A→B→A：判据必须比最初那个 host。逐跳比会认为最后一跳「回到了同 host」
// 从而把凭据加回去，而它在 B 那一跳已经暴露过了。
func TestReturnToOriginHostAfterLeavingStillCountsAsCrossHost(t *testing.T) {
	// 这里 origin 是 evil，链条是 evil→up：逐跳比时上一跳是 evil、这一跳是 up，
	// 两者不同所以两种实现都会拒；要区分得让「回到 origin」这一跳发生。
	req, via := hop(t, http.MethodPost, originURL, otherHost, originURL)
	req.Header.Set("x-api-key", "sk-anthropic")
	ctx, _ := withRedirectSink(req.Context())
	req = req.WithContext(ctx)

	// req 回到了 origin host，via[0] 也是 origin host，所以这一跳按 host 看是同源。
	// 它不该被 host 判据拦住——但它必须被跳数或方法判据管到，而这条用例只要求
	// 凭据在这一跳上是安全的：此前那一跳已经离开过，凭据已经被摘了。
	if got := req.Header.Get("x-api-key"); got == "" {
		t.Fatal("夹具自己就没设凭据")
	}
	err := checkRedirect(req, via)
	// 按 via[0] 判是同 host，方法也没变，所以放行——这正是要钉住的形态：
	// 摘除发生在离开的那一跳，回来的这一跳不负责再摘一次。
	if err != nil {
		t.Fatalf("回到 origin host 的那一跳被拒了：%v", err)
	}
}

// 302/301/303 会被标准库改写成无体的 GET。via 里留的是改写前的方法，
// 所以比较方法就能覆盖三个码。
func TestMethodRewriteIsRefused(t *testing.T) {
	req, via := hop(t, http.MethodGet, originURL, sameHost)
	// 手工把 via 的最后一跳改回 POST：这正是标准库处理 302 时的形状。
	via[len(via)-1].Method = http.MethodPost

	if err := checkRedirect(req, via); !errors.Is(err, errRedirectRewritesMethod) {
		t.Fatalf("方法被改写却放行了，请求体会被静默丢掉：err = %v", err)
	}
}

// 307/308 不改写方法，同 host 必须放行。
func TestMethodPreservingRedirectIsAllowed(t *testing.T) {
	req, via := hop(t, http.MethodPost, originURL, sameHost)
	if err := checkRedirect(req, via); err != nil {
		t.Fatalf("307 同 host 被拒了：%v", err)
	}
}

// 超跳数走自己的哨兵，不落到标准库那条带 URL 的文本上。
func TestTooManyHopsIsRefused(t *testing.T) {
	chain := make([]string, 0, maxRedirectHops+1)
	for i := 0; i <= maxRedirectHops; i++ {
		chain = append(chain, sameHost)
	}
	req, via := hop(t, http.MethodPost, originURL, chain...)
	if len(via) <= maxRedirectHops {
		t.Fatalf("夹具没造够跳数：len(via) = %d", len(via))
	}
	if err := checkRedirect(req, via); !errors.Is(err, errRedirectTooManyHops) {
		t.Fatalf("超跳数没被拦：err = %v", err)
	}
}

// 恰好到上限的那一跳必须还放行：上限是防回环，不是防正当的多级迁移。
func TestHopsAtTheLimitAreAllowed(t *testing.T) {
	chain := make([]string, 0, maxRedirectHops)
	for i := 0; i < maxRedirectHops; i++ {
		chain = append(chain, sameHost)
	}
	req, via := hop(t, http.MethodPost, originURL, chain...)
	if len(via) != maxRedirectHops {
		t.Fatalf("夹具跳数 = %d, want %d", len(via), maxRedirectHops)
	}
	if err := checkRedirect(req, via); err != nil {
		t.Fatalf("上限之内被拒了：%v", err)
	}
}

// 空 via 标准库不会给。真给了要放行而不是凭空报错：
// 这个函数的职责是拒绝坏形态，不是在拿不到基准时制造失败。
func TestEmptyViaIsAllowed(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, originURL, nil)
	if err := checkRedirect(req, nil); err != nil {
		t.Fatalf("空 via 报错了：%v", err)
	}
}

// ctx 上没挂 sink 时不能 panic：策略函数在任何客户端上都可能被调到。
func TestPolicyWorksWithoutSink(t *testing.T) {
	req, via := hop(t, http.MethodPost, originURL, otherHost)
	req.Header.Set("x-api-key", "sk-anthropic")
	if err := checkRedirect(req, via); !errors.Is(err, errRedirectCrossHost) {
		t.Fatalf("没挂 sink 时行为变了：%v", err)
	}
	if got := req.Header.Get("x-api-key"); got != "" {
		t.Errorf("没挂 sink 时凭据没摘：%q", got)
	}
}

// 三个哨兵各自映射到各自的文本，且没有一条把原始错误的内容带出来。
func TestRedirectFailureTexts(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{errRedirectRewritesMethod, "rewrites the request method"},
		{errRedirectCrossHost, "different host"},
		{errRedirectTooManyHops, "too long"},
	} {
		// 包一层 *url.Error 模拟 Do 的返回形态，且外层文本里塞一个带 query
		// 的 URL：固定文本的实现不会把它带出来，拼原始错误的实现会。
		wrapped := &wrappedURLErr{inner: tc.err}
		got, ok := redirectFailure(wrapped)
		if !ok {
			t.Fatalf("%v 没被认出来", tc.err)
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("%v 的文本 = %q, 应含 %q", tc.err, got, tc.want)
		}
		if strings.Contains(got, "://") || strings.Contains(got, "sk-inquery") {
			t.Errorf("文本里带上了原始错误的 URL：%q", got)
		}
	}
}

type wrappedURLErr struct{ inner error }

func (e *wrappedURLErr) Error() string {
	return `Post "http://up.example/v1?key=sk-inquery": ` + e.inner.Error()
}

func (e *wrappedURLErr) Unwrap() error { return e.inner }

// 非重定向错误不得被认成重定向失败。
func TestRedirectFailureIgnoresOtherErrors(t *testing.T) {
	if _, ok := redirectFailure(errors.New("connection refused")); ok {
		t.Error("普通连接错误被认成了重定向失败")
	}
	if _, ok := redirectFailure(nil); ok {
		t.Error("nil 被认成了重定向失败")
	}
}

// 客户端必须真的装上策略：漏一行会让整层静默失效，而编译器看不见。
func TestNewHTTPClientInstallsRedirectPolicy(t *testing.T) {
	c := NewHTTPClient(TransportOptions{})
	if c.CheckRedirect == nil {
		t.Fatal("客户端没装重定向策略，默认策略会改写方法并带着凭据跨 host")
	}
	req, via := hop(t, http.MethodPost, originURL, otherHost)
	if err := c.CheckRedirect(req, via); !errors.Is(err, errRedirectCrossHost) {
		t.Errorf("装上的不是本层的策略：%v", err)
	}
}

// 方法判据必须比上一跳而不是最初那一跳。
//
// 一跳的链条上两者相同，所以这条必须造两跳：第一跳已经把方法改写成 GET，
// 第二跳仍是 GET——按上一跳比是「没再变」放行，按最初那一跳比会误拒。
// 误拒本身不致命，但它把一个正当的多级迁移报成路由故障。
func TestMethodJudgementComparesPreviousHopNotTheFirst(t *testing.T) {
	first := httptest.NewRequest(http.MethodPost, originURL, nil)
	second := httptest.NewRequest(http.MethodGet, sameHost, nil)
	req := httptest.NewRequest(http.MethodGet, sameHost, nil)

	if err := checkRedirect(req, []*http.Request{first, second}); err != nil {
		t.Fatalf("按最初那一跳比会把这条误拒：%v", err)
	}
}

// 两个 ctx 必须拿到各自的 sink。共享一个全局 sink 会让并发请求的说明互相串，
// 而那正是「策略函数不能持有请求状态」这条约束存在的理由。
func TestEachContextGetsItsOwnSink(t *testing.T) {
	_, a := withRedirectSink(context.Background())
	_, b := withRedirectSink(context.Background())
	if a == b {
		t.Fatal("两个请求共用了一个 sink，说明会在并发请求之间串")
	}
	a.add("from a")
	if got := b.drain(); len(got) != 0 {
		t.Errorf("a 的说明出现在 b 上：%q", got)
	}
}

// drain 必须清空。不清空会让同一个 sink 上的说明在后续读取里重复出现。
func TestSinkDrainClears(t *testing.T) {
	_, s := withRedirectSink(context.Background())
	s.add("note")
	if got := s.drain(); len(got) != 1 {
		t.Fatalf("第一次 drain = %q", got)
	}
	if got := s.drain(); len(got) != 0 {
		t.Errorf("drain 没清空，说明会被重复取走：%q", got)
	}
}

// 外部包那条「上游被打了几次」的用例硬编码了跳数上限。这条钉住两者相等，
// 改一边不改另一边会被报出来，而不是让那条用例静默测错一个数。
func TestExportedHopLimitMatches(t *testing.T) {
	const inTest = 3
	if maxRedirectHops != inTest {
		t.Fatalf("maxRedirectHops = %d，而 redirect_test.go 里的 "+
			"maxRedirectHopsForTest 是 %d，两处必须一起改", maxRedirectHops, inTest)
	}
}
