package pipeline

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 本文件守三个纯判定：目标不可达、HTML 归因、响应头白名单。
//
// 纯函数级的用例在这里，端到端那一段在 attribution_test.go。分开是因为
// 这三个判定的边界（哪一类 DNS 失败算不可达、哪些头算可传）无法在端到端
// 用例里逐个摆出来——真实的 DNS 抖动与上游头族都不可控。

// ---- 目标不可达（判据 9–14）----

// wrapped 把错误按真实调用链裹起来：Do 返回的总是 *url.Error 裹 *net.OpError。
func wrapped(inner error) error {
	return &url.Error{
		Op:  "Post",
		URL: "https://api.example.com/v1/messages?key=sk-secret",
		Err: &net.OpError{Op: "dial", Net: "tcp", Err: inner},
	}
}

func TestNXDOMAINIsAnUnreachableTarget(t *testing.T) {
	err := wrapped(&net.DNSError{Err: "no such host", Name: "api.example.com", IsNotFound: true})
	if !isUnreachableTarget(err) {
		t.Error("NXDOMAIN 没判成目标不可达——归成 transport 的话这个目标" +
			"永远不计失败、永不冷却，调度层会一直把流量派给它")
	}
}

func TestDNSTimeoutStaysATransportError(t *testing.T) {
	err := wrapped(&net.DNSError{Err: "i/o timeout", Name: "api.example.com", IsTimeout: true})
	if isUnreachableTarget(err) {
		t.Error("DNS 超时被算成目标不可达——那是解析服务的问题，" +
			"算成目标失败会在 DNS 抖动时把整个账号池一起冷却")
	}
	if !isTransportError(err) {
		t.Error("DNS 超时也没算连接层故障，两边都不认就没人处置了")
	}
}

func TestDNSTemporaryFailureStaysATransportError(t *testing.T) {
	err := wrapped(&net.DNSError{Err: "server misbehaving", IsTemporary: true})
	if isUnreachableTarget(err) {
		t.Error("DNS 临时失败被算成目标不可达")
	}
}

func TestConnectionRefusedIsNotUnreachable(t *testing.T) {
	// 刻意如此：上游滚动重启时会短暂拒连，那是瞬时的。
	if isUnreachableTarget(wrapped(syscall.ECONNREFUSED)) {
		t.Error("连接被拒被算成目标不可达——滚动重启期间会把正在重启的" +
			"目标判死，而它几秒后就好了")
	}
}

func TestHostUnreachableIsAnUnreachableTarget(t *testing.T) {
	if !isUnreachableTarget(wrapped(syscall.EHOSTUNREACH)) {
		t.Error("EHOSTUNREACH 没判成目标不可达")
	}
}

func TestNetworkUnreachableIsAnUnreachableTarget(t *testing.T) {
	if !isUnreachableTarget(wrapped(syscall.ENETUNREACH)) {
		t.Error("ENETUNREACH 没判成目标不可达")
	}
}

func TestUnreachableIsNotConfusedWithNil(t *testing.T) {
	if isUnreachableTarget(nil) {
		t.Error("nil 被判成不可达")
	}
}

// 不可达的消息同样必须过 URL 净化：base_url 由调度层下发，
// 一些部署把 key 放在 query 里，而这句话会流到客户端可见的错误体。
func TestUnreachableMessageIsRedacted(t *testing.T) {
	err := wrapped(&net.DNSError{Err: "no such host", IsNotFound: true})
	msg := sanitizeTransportError(err)
	if strings.Contains(msg, "sk-secret") || strings.Contains(msg, "api.example.com/v1") {
		t.Errorf("净化后仍含 URL 或凭据: %q", msg)
	}
}

// ---- HTML 归因（判据 20–24）----

func TestDoctypeHTMLIsRecognized(t *testing.T) {
	if !looksLikeHTML([]byte("<!doctype html><html><body>blocked</body></html>")) {
		t.Error("小写 doctype 没认出来")
	}
}

func TestUppercaseDoctypeIsRecognized(t *testing.T) {
	// WAF 的拦截页两种写法都有，只认一种等于一半场景仍归错。
	if !looksLikeHTML([]byte("<!DOCTYPE HTML PUBLIC \"-//W3C//DTD HTML 4.01//EN\">")) {
		t.Error("大写 DOCTYPE 没认出来")
	}
}

func TestBareHTMLTagIsRecognized(t *testing.T) {
	if !looksLikeHTML([]byte("<HTML><head><title>403</title></head></HTML>")) {
		t.Error("没有 doctype 的裸 html 标签没认出来")
	}
}

func TestLeadingWhitespaceDoesNotHideHTML(t *testing.T) {
	if !looksLikeHTML([]byte("\r\n\t  <!doctype html>\n<html></html>")) {
		t.Error("前导空白让 HTML 判定失效了——反代插一个空行就绕过")
	}
}

func TestJSONIsNotMistakenForHTML(t *testing.T) {
	if looksLikeHTML([]byte(`{"type":"message","content":[]}`)) {
		t.Error("正常 JSON 被判成 HTML，会把真实的解码错误说成 WAF 拦截")
	}
}

// 内容里出现 <html> 但不在开头的不算：那是模型回的文本。
func TestHTMLInTheMiddleIsNotAPage(t *testing.T) {
	body := `{"content":[{"type":"text","text":"用 <html> 包起来"}]}`
	if looksLikeHTML([]byte(body)) {
		t.Error("模型回的内容里带 html 标签被判成拦截页——" +
			"那会把一次正常响应的解码失败归错")
	}
}

// ---- 响应头白名单（判据 25–30）----

func TestAnthropicRateLimitHeadersAreForwardable(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-remaining", "1500")
	h.Set("anthropic-ratelimit-unified-reset", "2026-09-19T14:00:00Z")
	got := forwardableHeaders(h)
	if got.Get("Anthropic-Ratelimit-Unified-Remaining") != "1500" {
		t.Errorf("unified-remaining 没被放行: %v", got)
	}
	if got.Get("Anthropic-Ratelimit-Unified-Reset") == "" {
		t.Errorf("unified-reset 没被放行: %v", got)
	}
}

func TestOpenAIRateLimitHeadersAreForwardable(t *testing.T) {
	h := http.Header{}
	h.Set("x-ratelimit-remaining-tokens", "39000")
	if forwardableHeaders(h).Get("X-Ratelimit-Remaining-Tokens") != "39000" {
		t.Error("x-ratelimit- 一族没被放行")
	}
}

func TestSetCookieIsNotForwardable(t *testing.T) {
	h := http.Header{}
	h.Set("Set-Cookie", "session=secret; HttpOnly")
	h.Set("anthropic-ratelimit-unified-remaining", "1")
	got := forwardableHeaders(h)
	if got.Get("Set-Cookie") != "" {
		t.Error("Set-Cookie 被回传给客户端了——白名单变成黑名单的典型症状")
	}
	if got.Get("Anthropic-Ratelimit-Unified-Remaining") == "" {
		t.Error("同时该放行的那个也没放行，说明筛选整体失效")
	}
}

// 上游的 x-request-id 不能按原名传：我们自己回显一个同名头，两个值会让
// 客户端拿到的追踪 ID 指向上游而不是我们的流水。但厂商侧的关联键本身
// 有价值（报障时对厂商日志用），所以改名回传，两个键都保住。
func TestUpstreamRequestIDIsForwardedRenamed(t *testing.T) {
	h := http.Header{}
	h.Set("x-request-id", "upstream-abc")
	got := forwardableHeaders(h)
	if got.Get("X-Request-Id") != "" {
		t.Error("上游的 x-request-id 按原名回传，会覆盖我们回显的请求 ID")
	}
	if got.Get(UpstreamRequestIDHeader) != "upstream-abc" {
		t.Errorf("改名回传缺席: %q = %q", UpstreamRequestIDHeader,
			got.Get(UpstreamRequestIDHeader))
	}
}

// Retry-After 刻意不走这条路：它已经由 codec/ratelimit 解析、
// 按我们自己的口径写出，两条路都写会产生两个值。
func TestRetryAfterIsNotInTheAllowlist(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "30")
	if forwardableHeaders(h).Get("Retry-After") != "" {
		t.Error("Retry-After 走了白名单，会与既有那条路写出两个值")
	}
}

func TestNoMatchingHeadersYieldsNothing(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	h.Set("Date", "Fri, 19 Sep 2026 13:00:00 GMT")
	if got := forwardableHeaders(h); len(got) != 0 {
		t.Errorf("没有限流头却筛出了 %v", got)
	}
}

// 多值头要整族带过去：限流族里有的头会重复出现。
func TestMultiValueHeadersArePreserved(t *testing.T) {
	h := http.Header{}
	h.Add("x-ratelimit-remaining", "1")
	h.Add("x-ratelimit-remaining", "2")
	if got := forwardableHeaders(h)["X-Ratelimit-Remaining"]; len(got) != 2 {
		t.Errorf("多值头被压成 %v", got)
	}
}

// 筛出来的必须是副本：直接引用上游的切片会让调用方写它时改到响应头。
func TestForwardableHeadersAreCopied(t *testing.T) {
	h := http.Header{}
	h.Set("x-ratelimit-remaining", "1")
	got := forwardableHeaders(h)
	got["X-Ratelimit-Remaining"][0] = "tampered"
	if h.Get("x-ratelimit-remaining") != "1" {
		t.Error("筛选结果与上游响应头共享底层数组")
	}
}

func TestApplyForwardedHeadersTolerantOfNil(t *testing.T) {
	// bridge 在失败路径上可能拿到 nil upstream，这里挂掉会把一次
	// 上游错误变成一次 panic。
	applyForwardedHeaders(newHeaderRecorder(), nil)
	applyForwardedHeaders(newHeaderRecorder(), &upstream{})
}

type headerRecorder struct{ h http.Header }

func newHeaderRecorder() *headerRecorder            { return &headerRecorder{h: http.Header{}} }
func (r *headerRecorder) Header() http.Header       { return r.h }
func (r *headerRecorder) Write([]byte) (int, error) { return 0, nil }
func (r *headerRecorder) WriteHeader(int)           {}

func TestApplyForwardedHeadersWritesEveryValue(t *testing.T) {
	up := &upstream{forwardHeaders: http.Header{
		"X-Ratelimit-Remaining": {"1", "2"},
	}}
	w := newHeaderRecorder()
	applyForwardedHeaders(w, up)
	if got := w.Header()["X-Ratelimit-Remaining"]; len(got) != 2 {
		t.Errorf("写出 %v，多值头没全写", got)
	}
}

// ---- SideEffectRisk 标记（判据 5 的一半）----

func TestWithSideEffectRiskMarksAndPassesThrough(t *testing.T) {
	err := ir.NewError(ir.ErrTimeout, 0, "", "boom")
	if got := withSideEffectRisk(err, true); !got.SideEffectRisk {
		t.Error("标记没生效")
	}
	if got := withSideEffectRisk(ir.NewError(ir.ErrTimeout, 0, "", "boom"), false); got.SideEffectRisk {
		t.Error("没发出去的请求被标了风险，会让一次拨号失败直接失败而不是换目标")
	}
}

func TestWithSideEffectRiskTolerantOfNil(t *testing.T) {
	if got := withSideEffectRisk(nil, true); got != nil {
		t.Errorf("nil 进来变成了 %v", got)
	}
}

// SideEffectRisk 与 Retryable 必须是两个独立维度：合并成一个之后
// 「将来放宽某一类」就没有落点，而且会把 kind 的语义搅进来。
func TestSideEffectRiskIsIndependentOfRetryable(t *testing.T) {
	err := withSideEffectRisk(ir.NewError(ir.ErrTimeout, 0, "", "boom"), true)
	if !err.Retryable {
		t.Error("标风险顺手把 Retryable 改了——那是另一个问题的答案")
	}
	if !err.SideEffectRisk {
		t.Error("风险标记丢了")
	}
}

// 确保 errors.As 能在真实的多层包装里找到 DNSError：判定链断在
// 包装层上的话，上面所有用例都还绿，而生产里一条都不生效。
func TestUnreachableSeesThroughDeepWrapping(t *testing.T) {
	inner := &net.DNSError{Err: "no such host", IsNotFound: true}
	deep := fmt.Errorf("layer1: %w", fmt.Errorf("layer2: %w", wrapped(inner)))
	if !isUnreachableTarget(deep) {
		t.Error("多层包装下认不出 DNSError")
	}
	var probe *net.DNSError
	if !errors.As(deep, &probe) {
		t.Error("errors.As 本身就找不到，用例前提不成立")
	}
}
