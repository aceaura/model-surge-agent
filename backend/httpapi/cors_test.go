package httpapi_test

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/httpapi"
)

// 这一组测的是「浏览器里的 SDK 能不能打进来」。CORS 是纯浏览器侧的强制，
// 服务端的全部职责就是把头发对。

// newCORSFixture 起一个带来源白名单的服务。空切片表示放开所有来源。
func newCORSFixture(t *testing.T, origins ...string) *fixture {
	t.Helper()
	return newFixtureTuned(t, nil, func(s *httpapi.Server) {
		s.CORSOrigins = origins
	})
}

func TestPreflightIsAnsweredWithoutRouting(t *testing.T) {
	// 预检必须在路由之前答掉：OPTIONS 落到 ServeMux 会得到 405，
	// 再被受理面那套改写成错误信封，浏览器据此判定跨域失败。
	f := newCORSFixture(t)
	for _, path := range []string{"/v1/messages", "/v1/models", "/v1/chat/completions",
		"/no/such/path"} {
		resp := f.do(t, http.MethodOptions, path, map[string]string{
			"Origin":                         "https://app.example.com",
			"Access-Control-Request-Method":  "POST",
			"Access-Control-Request-Headers": "content-type, x-api-key",
		})
		if resp.Code != http.StatusNoContent {
			t.Errorf("%s: status = %d, want 204: %s", path, resp.Code, resp.Body.String())
		}
		if resp.Body.Len() != 0 {
			t.Errorf("%s: preflight must have no body, got %s", path, resp.Body.String())
		}
	}
}

func TestPreflightIsNotRecorded(t *testing.T) {
	// 预检不是一次业务请求。记进流水会让每个浏览器请求都多出一条，
	// 而运维统计的是「客户端发了多少次对话」。
	f := newCORSFixture(t)
	f.do(t, http.MethodOptions, "/v1/messages", map[string]string{
		"Origin": "https://app.example.com",
	})
	if recs := f.records.all(); len(recs) != 0 {
		t.Errorf("records = %d, want 0: %+v", len(recs), recs)
	}
	if f.upstreamCalls() != 0 {
		t.Error("preflight must not reach the upstream")
	}
}

func TestPreflightCarriesTheFullHeaderAllowList(t *testing.T) {
	// 逐个列举时最容易漏的就是厂商专有头。漏一个，浏览器端那家的 SDK
	// 预检必然失败，而失败信息只说「header not allowed」。
	f := newCORSFixture(t)
	resp := f.do(t, http.MethodOptions, "/v1/messages", map[string]string{
		"Origin": "https://app.example.com",
	})
	allow := strings.ToLower(resp.Header().Get("Access-Control-Allow-Headers"))
	for _, h := range []string{
		"content-type", "authorization",
		"x-api-key", "anthropic-version", "anthropic-beta", "x-goog-api-key",
		"x-stainless-lang", "x-stainless-retry-count",
	} {
		if !strings.Contains(allow, h) {
			t.Errorf("allow-headers missing %q: %s", h, allow)
		}
	}
	if !strings.Contains(resp.Header().Get("Access-Control-Allow-Methods"), "POST") {
		t.Errorf("allow-methods must include POST: %s", resp.Header().Get("Access-Control-Allow-Methods"))
	}
	// 没有 max-age 时浏览器对每个请求都先预检一次，延迟翻倍。
	if resp.Header().Get("Access-Control-Max-Age") == "" {
		t.Error("max-age must be set, otherwise every request preflights again")
	}
	// 不暴露则浏览器里的 JS 读不到请求 ID，报障时无从对账。
	if !strings.EqualFold(resp.Header().Get("Access-Control-Expose-Headers"), "X-Request-Id") {
		t.Errorf("expose-headers = %q, want X-Request-Id",
			resp.Header().Get("Access-Control-Expose-Headers"))
	}
}

func TestAnyOriginSendsWildcardWithoutCredentials(t *testing.T) {
	// 浏览器拒绝 `*` 与 Allow-Credentials 的组合，整个响应会被丢掉。
	// 两个都发等于跨域完全不通，而错误信息不会指向这里。
	f := newCORSFixture(t)
	resp := f.do(t, http.MethodOptions, "/v1/messages", map[string]string{
		"Origin": "https://app.example.com",
	})
	if got := resp.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("allow-origin = %q, want *", got)
	}
	if got := resp.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("allow-credentials = %q, must be absent alongside *", got)
	}
}

func TestExplicitWildcardBehavesLikeNoConfig(t *testing.T) {
	// 配 ["*"] 与不配是同一件事。两者行为不一致会让部署方以为自己
	// 配紧了，其实没有。
	f := newCORSFixture(t, "*")
	resp := f.do(t, http.MethodOptions, "/v1/messages", map[string]string{
		"Origin": "https://app.example.com",
	})
	if got := resp.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("allow-origin = %q, want *", got)
	}
	if got := resp.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("allow-credentials = %q, must be absent alongside *", got)
	}
}

func TestWhitelistedOriginIsEchoedWithCredentials(t *testing.T) {
	// 配了具体来源才能带凭据。回显来源时必须声明 Vary：否则中间缓存
	// 会把一个来源的响应喂给另一个来源。
	f := newCORSFixture(t, "https://ok.example.com")
	resp := f.do(t, http.MethodOptions, "/v1/messages", map[string]string{
		"Origin": "https://ok.example.com",
	})
	if got := resp.Header().Get("Access-Control-Allow-Origin"); got != "https://ok.example.com" {
		t.Errorf("allow-origin = %q, want the origin echoed", got)
	}
	if got := resp.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("allow-credentials = %q, want true", got)
	}
	if !strings.Contains(resp.Header().Get("Vary"), "Origin") {
		t.Errorf("Vary = %q, must include Origin", resp.Header().Get("Vary"))
	}
}

func TestOriginMatchIsCaseInsensitive(t *testing.T) {
	// 主机名大小写不敏感。大小写不同就判不在白名单会让部署方对着一条
	// 看起来完全一样的配置排查。
	f := newCORSFixture(t, "https://OK.example.com")
	resp := f.do(t, http.MethodOptions, "/v1/messages", map[string]string{
		"Origin": "https://ok.example.com",
	})
	if got := resp.Header().Get("Access-Control-Allow-Origin"); got == "" {
		t.Error("origin differing only in case must still match")
	}
}

func TestUnlistedOriginIsStillServed(t *testing.T) {
	// 不在白名单：不发 CORS 头，但请求照常处理。服务端多拦一层只会让
	// 非浏览器客户端（它们不看 CORS）莫名被拒。
	f := newCORSFixture(t, "https://ok.example.com")
	resp := f.post(t, "/v1/messages", requestBodies["anthropic"], map[string]string{
		"Origin": "https://evil.example.com",
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("allow-origin = %q, must be absent for an unlisted origin", got)
	}
	// 即使不放行也要声明 Vary：响应内容与 Origin 有关（有无 CORS 头）。
	if !strings.Contains(resp.Header().Get("Vary"), "Origin") {
		t.Errorf("Vary = %q, must include Origin", resp.Header().Get("Vary"))
	}
}

func TestNonBrowserRequestGetsNoCORSHeaders(t *testing.T) {
	// 无 Origin 的请求是非浏览器客户端，发 CORS 头只是噪声。
	f := newCORSFixture(t)
	resp := f.post(t, "/v1/messages", requestBodies["anthropic"], nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
	}
	for _, h := range []string{
		"Access-Control-Allow-Origin", "Access-Control-Allow-Headers",
		"Access-Control-Allow-Methods", "Access-Control-Max-Age",
	} {
		if got := resp.Header().Get(h); got != "" {
			t.Errorf("%s = %q, must be absent without an Origin", h, got)
		}
	}
}

func TestNormalResponsesAlsoCarryCORSHeaders(t *testing.T) {
	// 只答预检不够：真正那次请求的响应也得带 Allow-Origin，
	// 否则浏览器在预检通过之后仍然把结果丢掉。
	f := newCORSFixture(t)
	resp := f.post(t, "/v1/messages", requestBodies["anthropic"], map[string]string{
		"Origin": "https://app.example.com",
	})
	if got := resp.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("POST allow-origin = %q, want *", got)
	}
	list := f.getWith(t, "/v1/models", map[string]string{"Origin": "https://app.example.com"})
	if got := list.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("GET allow-origin = %q, want *", got)
	}
}

func TestCORSHeadersSurviveErrorResponses(t *testing.T) {
	// 出错的响应更需要 CORS 头：没有它，浏览器里的 JS 读不到状态码与
	// 错误体，只看到一句 network error——正是最该看清错误的时候。
	f := newCORSFixture(t)
	cases := map[string]string{
		"/v1/messages":  `{"messages":[]}`,
		"/no/such/path": `{}`,
	}
	for path, body := range cases {
		resp := f.post(t, path, body, map[string]string{"Origin": "https://app.example.com"})
		if resp.Code == http.StatusOK {
			t.Fatalf("%s: want a failure to test against, got 200", path)
		}
		if got := resp.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("%s: allow-origin = %q on a %d response", path, got, resp.Code)
		}
	}
}

// TestVendorHeadersAreInTheAllowList 是源码级守卫。
//
// 上面那条测的是当前白名单里有这些头；这条盯的是「有人日后精简这个列表」
// 时会被挡住。厂商专有头一旦被当成冗余删掉，浏览器端那家 SDK 直接不可用。
func TestVendorHeadersAreInTheAllowList(t *testing.T) {
	src, err := os.ReadFile("cors.go")
	if err != nil {
		t.Fatalf("read cors.go: %v", err)
	}
	text := strings.ToLower(string(src))
	for _, h := range []string{
		"x-api-key", "anthropic-version", "anthropic-beta", "x-goog-api-key",
		"x-stainless-",
	} {
		if !strings.Contains(text, h) {
			t.Errorf("cors.go 丢了 %q：浏览器端那家 SDK 会预检失败", h)
		}
	}
	// 通配 `*` 与 Allow-Credentials 不能并存，这条不变量写在代码里。
	if strings.Contains(text, `h.set("access-control-allow-headers", "*")`) {
		t.Error("不能用 `*` 放行请求头：它与 Allow-Credentials 并存时浏览器拒绝整个响应")
	}
}
