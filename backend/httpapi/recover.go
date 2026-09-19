package httpapi

import (
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// withRecover 兜住处理链里的 panic。
//
// 不兜的后果具体到一种症状：任一 codec 在流中途 panic，连接直接断，客户端
// 看到的是一个**未闭合的流**——SDK 那边表现成解析卡住或超时，而不是一个
// 能报给人看的错误。两个参考实现都包了恢复中间件。
//
// 挂在最外层（withCORS 之外）：CORS 与访问日志自己也可能 panic，包在它们
// 里面就漏了。管理面走同一条链，一并覆盖。
func (s *Server) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pw := &panicWriter{ResponseWriter: w}
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			// ErrAbortHandler 是标准库表达「故意中断这个响应」的方式，
			// 恢复它等于把一个刻意的中断改写成一次错误。原样抛回去。
			if v == http.ErrAbortHandler {
				panic(v)
			}
			// 栈必须打：panic 的定位信息只在栈里，只记一个值等于知道
			// 「炸了」但不知道在哪。
			s.log().Error("panic recovered",
				"method", r.Method, "path", r.URL.Path,
				"panic", fmt.Sprint(v), "stack", string(debug.Stack()))
			s.writePanic(pw, r)
		}()
		next.ServeHTTP(pw, r)
	})
}

// writePanic 按「响应写到哪一步了」决定怎么收尾。
func (s *Server) writePanic(pw *panicWriter, r *http.Request) {
	err := ir.NewError(ir.ErrInternal, http.StatusInternalServerError, "",
		"internal error while handling the request")

	if !pw.written {
		// 还没写响应头：能给出正确的 HTTP 状态码与本服务的错误信封。
		// 管理面有自己的错误形状，不套数据面信封——理由同 withRejectEnvelope。
		if isAdminPath(r.URL.Path) {
			writeJSON(pw, http.StatusInternalServerError,
				map[string]string{"error": err.Message})
			return
		}
		inbound, _ := codec.Inbound(protocolForPath(r.URL.Path))
		writeIRErrorStatus(pw, inbound, err, http.StatusInternalServerError)
		return
	}

	// 已经开始写了：状态码收不回来，改状态码会让客户端看到 200 配 500 体。
	// 尽力发一个流内错误帧让客户端的状态机收束——它据此知道这轮不可用，
	// 不会把残缺内容当完整回答存进历史。
	if isAdminPath(r.URL.Path) {
		return
	}
	inbound, ok := codec.Inbound(protocolForPath(r.URL.Path))
	if !ok {
		return
	}
	for _, f := range inbound.RenderStreamError(err) {
		if _, werr := pw.Write(f); werr != nil {
			return
		}
	}
	flushWriter(pw)
}

func isAdminPath(path string) bool {
	return strings.HasPrefix(path, "/admin/")
}

func flushWriter(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// panicWriter 记下响应是否已经开始写出。
//
// 只记这一位，不记状态码：恢复路径要判的就是「还能不能定状态码」。
type panicWriter struct {
	http.ResponseWriter
	written bool
}

func (w *panicWriter) WriteHeader(status int) {
	w.written = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *panicWriter) Write(b []byte) (int, error) {
	w.written = true
	return w.ResponseWriter.Write(b)
}

// Flush 透传，否则 SSE 会攒到流结束才一次性发出。
func (w *panicWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 透传，否则数据面给每次写推的写 deadline 会静默失效。
// 理由同 statusRecorder.Unwrap。
func (w *panicWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
