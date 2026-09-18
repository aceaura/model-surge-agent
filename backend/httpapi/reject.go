package httpapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
	"github.com/aceaura/model-surge-agent/backend/pipeline"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
)

// protocolForPath 按路径推断该用哪个协议的错误信封。
//
// 推断不出时取 anthropic：数据面必须回一个客户端能解析的东西，
// 而认错协议的代价（SDK 报字段不认识）远小于回一段纯文本（SDK 报解码失败）。
func protocolForPath(path string) string {
	p := strings.ToLower(path)
	switch {
	case strings.Contains(p, "/chat/completions"):
		return codec.ProtocolChatCompletions
	case strings.Contains(p, "/responses"):
		return codec.ProtocolResponses
	case strings.Contains(p, "/messages"):
		return codec.ProtocolAnthropic
	case strings.HasPrefix(p, "/openai/"):
		return codec.ProtocolChatCompletions
	default:
		return codec.ProtocolAnthropic
	}
}

// reject 回一个受理面拒绝：按客户端协议渲染信封，并记一条流水。
//
// 记流水是这条路径存在的主要理由。客户端报「连不上」时，服务端若查不到
// 任何记录，就无法判断请求到底有没有到达本服务——而受理面的拒绝恰好全都
// 发生在 pipeline 之前，pipeline 那套记流水一条都不会触发。
func (s *Server) reject(w http.ResponseWriter, r *http.Request, protocol string, err *ir.Error) {
	inbound, _ := codec.Inbound(protocol)
	requestID := requestID(r)
	w.Header().Set(headerRequestID, requestID)
	status := writeIRError(w, inbound, err)
	s.recordRejection(r, requestID, protocol, status, err)
}

// recordRejection 把受理面的拒绝记进流水。
//
// 刻意不带任何目标信息、也不上报调度层：这一刻还没选过目标，硬编一个占位
// 账号会让某个真实账号无端累计失败。attempts 记 0 而非 1，运维据此能把
// 「从未打上游」与「打了一次但失败」分开。
func (s *Server) recordRejection(r *http.Request, requestID, protocol string, status int, err *ir.Error) {
	if s.Recorder == nil {
		return
	}
	s.Recorder.Record(pipeline.Record{
		RequestID:       requestID,
		At:              time.Now().UTC(),
		InboundProtocol: protocol,
		Path:            r.URL.Path,
		Outcome:         relayclient.OutcomeAbnormal,
		StatusCode:      status,
		Attempts:        0,
		ErrorCode:       string(err.Kind),
		ErrorMessage:    err.Message,
	})
}

// withRejectEnvelope 把路由层回的 404/405 换成客户端协议的错误信封。
//
// 包在 mux 外面而不是用 mux.Handle("/") 兜底：后者会把方法不匹配也吃成
// 404，而标准库的 405 自带 Allow 头——那个头三个参考实现一个都没有，
// 没理由为了统一错误形状把它丢掉。
func (s *Server) withRejectEnvelope(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 管理面有自己的错误形状，套上数据面的信封会让管理客户端解析失败。
		if strings.HasPrefix(r.URL.Path, "/admin/") {
			next.ServeHTTP(w, r)
			return
		}
		rw := &rejectWriter{ResponseWriter: w, server: s, req: r}
		next.ServeHTTP(rw, r)
	})
}

// rejectWriter 截住 404/405 的响应体。
//
// 只认这两个码：它们是路由层自己产生的，处理函数根本没被调用过，所以
// 响应体必定是标准库那句纯文本。处理函数自己回的 404（管理面的
// 「no such request」）走不到这里，因为管理面在上一层就被放过了。
type rejectWriter struct {
	http.ResponseWriter
	server *Server
	req    *http.Request

	rewriting bool
	done      bool
}

func (w *rejectWriter) WriteHeader(status int) {
	if w.done {
		return
	}
	w.done = true
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		w.rewriting = true
		w.server.rejectRouting(w.ResponseWriter, w.req, status)
		return
	}
	w.ResponseWriter.WriteHeader(status)
}

// Write 在改写模式下丢掉标准库那句纯文本，其余原样透传。
func (w *rejectWriter) Write(b []byte) (int, error) {
	if w.rewriting {
		// 假装写成功：返回错误会让标准库的 Error() 在日志里报一次
		// 无意义的写失败，而我们是故意不写的。
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}

// Flush 透传，否则 SSE 会攒到流结束才一次性发出。
func (w *rejectWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// rejectRouting 渲染路由层拒绝的响应体。
//
// 消息里带上方法与路径：客户端据此能直接看出是 base_url 配错（路径不认识）
// 还是方法用错，不必再去翻文档对照。
func (s *Server) rejectRouting(w http.ResponseWriter, r *http.Request, status int) {
	var err *ir.Error
	switch status {
	case http.StatusMethodNotAllowed:
		err = ir.NewError(ir.ErrInvalidRequest, status, "",
			"method "+r.Method+" is not allowed on "+r.URL.Path)
	default:
		err = ir.NewError(ir.ErrNotFound, status, "",
			"no such endpoint: "+r.Method+" "+r.URL.Path)
	}

	protocol := protocolForPath(r.URL.Path)
	inbound, _ := codec.Inbound(protocol)
	requestID := requestID(r)
	w.Header().Set(headerRequestID, requestID)
	// 用路由层给的状态码，不用信封推导出来的：Allow 头已经随 405 发出，
	// 体里的码与头不一致会让客户端两边对不上。
	writeIRErrorStatus(w, inbound, err, status)
	s.recordRejection(r, requestID, protocol, status, err)
}
