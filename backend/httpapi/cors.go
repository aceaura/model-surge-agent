package httpapi

import (
	"net/http"
	"strings"
)

// corsAllowHeaders 是预检放行的请求头。
//
// 白名单而非通配 `*`：通配与 Allow-Credentials 并存时浏览器会拒绝整个
// 响应，而部署方是否配 credentials 我们预知不了。逐个列出的代价是新 SDK
// 加了新头要跟改，收益是行为可预测。
//
// anthropic-version 与 anthropic-beta 必须在列：sub2api 的白名单
// （middleware/cors.go:53-64）漏了这两个，浏览器端 Anthropic SDK 在它
// 上面预检必然失败——这是逐个列举时最容易漏的一类。
var corsAllowHeaders = []string{
	"Content-Type", "Content-Encoding", "Content-Length",
	"Authorization", "Accept", "Accept-Encoding",
	"x-api-key", "anthropic-version", "anthropic-beta",
	"x-goog-api-key", "X-Request-Id",
	// OpenAI 的 Node/Python SDK 发这一族头。取值来自 sub2api 的实测清单。
	"x-stainless-arch", "x-stainless-lang", "x-stainless-os",
	"x-stainless-package-version", "x-stainless-runtime",
	"x-stainless-runtime-version", "x-stainless-retry-count",
	"x-stainless-timeout", "x-stainless-async", "x-stainless-helper-method",
	"x-stainless-poll-helper", "x-stainless-custom-event",
}

// corsMaxAge 是预检结果的缓存秒数。
// 不设它的话浏览器对每个请求都先发一次 OPTIONS，延迟直接翻倍。
const corsMaxAge = "86400"

var corsAllowHeadersValue = strings.Join(corsAllowHeaders, ", ")

// allowsAnyOrigin 报告配置是否放开所有来源。nil 也算放开。
func (s *Server) allowsAnyOrigin() bool {
	if len(s.CORSOrigins) == 0 {
		return true
	}
	for _, o := range s.CORSOrigins {
		if o == "*" {
			return true
		}
	}
	return false
}

func (s *Server) originAllowed(origin string) bool {
	for _, o := range s.CORSOrigins {
		if strings.EqualFold(o, origin) {
			return true
		}
	}
	return false
}

// setCORSHeaders 按来源配置挂上 CORS 头。
//
// 无 Origin 的请求一个头都不发：那是非浏览器客户端，发了只是噪声。
func (s *Server) setCORSHeaders(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}
	h := w.Header()
	switch {
	case s.allowsAnyOrigin():
		h.Set("Access-Control-Allow-Origin", "*")
		// 刻意不发 Allow-Credentials：浏览器拒绝 `*` 与 credentials 的
		// 组合，发了只会让「带凭据的跨域请求为什么失败」更难查。
		// 要用凭据就配具体的来源白名单。
	case s.originAllowed(origin):
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Allow-Credentials", "true")
		// 回显了来源就必须声明 Vary：否则中间缓存会把一个来源的响应
		// 喂给另一个来源。
		h.Add("Vary", "Origin")
	default:
		// 不在白名单：不发 CORS 头，但请求照常处理。CORS 是浏览器侧的
		// 强制，服务端多拦一层只会让非浏览器客户端莫名被拒。
		h.Add("Vary", "Origin")
		return
	}
	h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", corsAllowHeadersValue)
	// 暴露请求 ID：客户端报障要对账，不暴露则浏览器里的 JS 读不到这个头。
	h.Set("Access-Control-Expose-Headers", headerRequestID)
	h.Set("Access-Control-Max-Age", corsMaxAge)
}

// withCORS 处理预检并给正常响应挂上 CORS 头。
//
// 包在最外层：预检必须在路由之前答掉，否则 OPTIONS 会落到 ServeMux 的
// 405、被受理面拒绝那套改写成错误信封，浏览器据此判定跨域失败。
func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.setCORSHeaders(w, r)
		if r.Method == http.MethodOptions {
			// 预检不进路由也不记流水：它不是一次业务请求，记进去会让
			// 流水里每个浏览器请求都多出一条。
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
