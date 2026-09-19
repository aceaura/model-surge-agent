package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	_ "github.com/aceaura/model-surge-agent/backend/codec/anthropic"
)

// panicServer 装一台在处理中 panic 的服务。
// stream 为真表示 panic 前先写出 200 与一个帧。
func panicServer(stream bool, value any) http.Handler {
	srv := &Server{}
	return srv.withRecover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("event: message_start\ndata: {}\n\n"))
		}
		panic(value)
	}))
}

// panic 不得让连接无声断掉：客户端要么收到完整响应，要么收到明确的错误。
func TestPanicBeforeHeadersBecomesErrorEnvelope(t *testing.T) {
	w := httptest.NewRecorder()
	panicServer(false, "boom").ServeHTTP(w,
		httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d, want 500", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"error"`) {
		t.Errorf("响应体不是错误信封：%s", w.Body)
	}
	// panic 值不得回给客户端：它可能含内部细节。
	if strings.Contains(w.Body.String(), "boom") {
		t.Errorf("panic 值泄漏给客户端：%s", w.Body)
	}
}

// headerCounter 数 WriteHeader 被调了几次。
//
// 不能只看 httptest.ResponseRecorder 的 Code：它对第二次 WriteHeader
// 直接忽略，于是「已开始写流后又改状态码」这个 bug 在它上面看不出来。
// 真实的 http.response 会打一行 "superfluous WriteHeader" 并同样忽略——
// 也就是说这个 bug 不改状态码，改的是响应体：本该是流内错误帧的地方
// 变成了一份错误信封 JSON。
type headerCounter struct {
	http.ResponseWriter
	calls int
}

func (w *headerCounter) WriteHeader(status int) {
	w.calls++
	w.ResponseWriter.WriteHeader(status)
}

// 已经开始写流时状态码收不回来，不得改它，但要尽力发一个错误帧。
func TestPanicMidStreamKeepsStatusAndEmitsErrorFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &headerCounter{ResponseWriter: rec}
	panicServer(true, "midway").ServeHTTP(w,
		httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，已开始写流后不得改它", rec.Code)
	}
	// 只该有处理器自己那一次：恢复路径再调一次就是 superfluous WriteHeader。
	if w.calls != 1 {
		t.Errorf("WriteHeader 被调了 %d 次，want 1（恢复路径不得再定状态码）", w.calls)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "error") {
		t.Errorf("未发出流内错误帧：%s", body)
	}
	// 收尾必须是 SSE 帧而不是一份错误信封 JSON：后者混在流里客户端解不动。
	if !strings.Contains(body, "event: error") {
		t.Errorf("收尾不是流内错误帧：%s", body)
	}
	// 先写出的那一帧要还在：错误帧是追加的，不是替换的。
	if !strings.Contains(body, "message_start") {
		t.Errorf("已发出的内容被丢了：%s", body)
	}
}

// ErrAbortHandler 是标准库表达「故意中断」的方式，恢复它等于改写一个刻意的中断。
func TestPanicRecoverLetsErrAbortHandlerThrough(t *testing.T) {
	defer func() {
		if v := recover(); v != http.ErrAbortHandler {
			t.Errorf("ErrAbortHandler 被吞了：recover() = %v", v)
		}
	}()
	panicServer(false, http.ErrAbortHandler).ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))
	t.Error("ErrAbortHandler 没有抛回来")
}

// 管理面有自己的错误形状，套上数据面信封会让管理客户端解析失败。
func TestPanicOnAdminPathUsesPlainError(t *testing.T) {
	w := httptest.NewRecorder()
	panicServer(false, "admin boom").ServeHTTP(w,
		httptest.NewRequest(http.MethodGet, "/admin/requests", nil))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d, want 500", w.Code)
	}
	// anthropic 信封带 "type" 键，管理面的只有 "error"。
	if strings.Contains(w.Body.String(), `"type"`) {
		t.Errorf("管理面被套上了数据面信封：%s", w.Body)
	}
}

// 管理面在流中途 panic 时不发数据面的错误帧：那个形状它解不动。
func TestPanicMidStreamOnAdminPathWritesNothingMore(t *testing.T) {
	w := httptest.NewRecorder()
	panicServer(true, "admin mid").ServeHTTP(w,
		httptest.NewRequest(http.MethodGet, "/admin/requests", nil))

	if strings.Contains(w.Body.String(), `"type"`) {
		t.Errorf("管理面收到了数据面错误帧：%s", w.Body)
	}
}

// 恢复中间件不得干扰正常响应。
func TestRecoverPassesThroughNormalResponses(t *testing.T) {
	srv := &Server{}
	h := srv.withRecover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("fine"))
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/whatever", nil))

	if w.Code != http.StatusTeapot || w.Body.String() != "fine" {
		t.Errorf("正常响应被改了：code=%d body=%q", w.Code, w.Body)
	}
}

// panicOnAccessLog 是一个只在访问日志那条记录上 panic 的 slog handler。
//
// 放过恢复路径自己的那条 "panic recovered"：否则恢复中间件在记日志时
// 再次 panic，测的就不是链的顺序了。
type panicOnAccessLog struct{}

func (panicOnAccessLog) Enabled(context.Context, slog.Level) bool { return true }

func (panicOnAccessLog) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "request" {
		panic("access log exploded")
	}
	return nil
}

func (h panicOnAccessLog) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h panicOnAccessLog) WithGroup(string) slog.Handler      { return h }

// 访问日志层自己 panic 时也必须被兜住：它在 next 返回之后才记日志，
// 恢复若挂在它之内就漏了这一段，连接会无声断掉。
//
// 这个断言只在恢复位于访问日志之外时成立——把恢复挪进链内侧，
// 这个 panic 就会一路抛到 ServeHTTP 之外。
func TestPanicInAccessLogLayerIsRecovered(t *testing.T) {
	srv := &Server{AccessLog: true, Log: slog.New(panicOnAccessLog{})}
	h := srv.Handler()
	w := httptest.NewRecorder()
	defer func() {
		if v := recover(); v != nil {
			t.Fatalf("panic 逃出了处理链：%v（恢复挂在访问日志之内）", v)
		}
	}()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
}

// 恢复必须挂在处理链最外层：CORS 与访问日志自己也可能 panic。
// 断言链的形状而不是构造一个会 panic 的 CORS 实现：后者要改生产代码。
func TestRecoverIsOutermostInHandlerChain(t *testing.T) {
	srv := &Server{AccessLog: true, CORSOrigins: []string{"*"}}
	h := srv.Handler()
	w := httptest.NewRecorder()
	// Pipeline 为 nil：dataPlane 会 nil 解引用 panic。这一层在 CORS 与
	// 访问日志之内，能走到这里说明恢复至少在它们之外。
	r := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://example.com")
	h.ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d, want 500", w.Code)
	}
	// CORS 头仍在：恢复在它之外，所以它已经挂好了头才轮到 panic。
	if w.Header().Get("Access-Control-Allow-Origin") == "" {
		t.Error("CORS 头丢了，说明恢复挂在了 CORS 之内")
	}
}

// panicWriter 必须转发 Flush 与 Unwrap：不转发的话 SSE 会攒到流结束才发、
// 写 deadline 会静默失效。本仓库在 statusRecorder 与 rejectWriter 上各踩过一次。
func TestPanicWriterForwardsFlushAndUnwrap(t *testing.T) {
	inner := httptest.NewRecorder()
	pw := &panicWriter{ResponseWriter: inner}

	if _, ok := any(pw).(http.Flusher); !ok {
		t.Fatal("panicWriter 不是 Flusher")
	}
	pw.Flush()
	if !inner.Flushed {
		t.Error("Flush 没有透传")
	}
	if got := pw.Unwrap(); got != http.ResponseWriter(inner) {
		t.Error("Unwrap 没有给出内层 writer")
	}
}
