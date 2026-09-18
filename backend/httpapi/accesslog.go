package httpapi

import (
	"net/http"
	"time"
)

// withAccessLog 记一行访问日志。
//
// 绝不打请求体与凭据头：请求体是用户的对话内容，头里有客户端密钥。
// 排查问题所需的关联信息在 request_log 里，那张表本身就不含这两样。
func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.AccessLog {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		s.log().Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"ms", time.Since(start).Milliseconds(),
		)
	})
}

// statusRecorder 记下状态码。同时透传 Flush：数据面靠它把 SSE 逐帧推出去，
// 包一层却不转发的话客户端会等到流结束才一次性收到全部内容。
// Unwrap 同理但更隐蔽，见其注释。
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (w *statusRecorder) WriteHeader(status int) {
	if w.written {
		return
	}
	w.written = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	w.written = true
	return w.ResponseWriter.Write(b)
}

func (w *statusRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让 http.ResponseController 能找到真实连接。
//
// 数据面靠 ResponseController 给每次写推写 deadline 来挡住慢客户端，
// 而它是沿 Unwrap 方法链向下找的。只做 struct embedding 虽然满足了
// ResponseWriter 接口，但 ResponseController 认不出被包住的是什么，
// 会返回 ErrNotSupported——deadline 代码写了也不生效，且不报错、无症状。
func (w *statusRecorder) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
