package pipeline_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
	"github.com/aceaura/model-surge-agent/backend/relaymock"
)

// 本文件守「上游先在流内说明了原因再断流」这一形态。判据 16–19。
//
// 此前两条拆流分支都只报兜底错误（channel 干净关闭报「流没有终止符就结束了」、
// 读错误报「upstream stream broke」，两者 kind 都是 upstream），于是一次流内
// 429 被记成目标故障累计它的失败计数。运维看到的是一次上游抖动，而真相是
// 这个账号已被限流、该退避。

// abruptAfterError 发若干帧后直接掐断 TCP，不给 chunk 终止符。
//
// 必须走 Hijack 而不是普通 handler 返回：后者由 net/http 干净收尾，读侧看到的
// 是 channel 关闭那条分支；掐断连接才走 scanner 报读错误那条。两条分支
// 都要覆盖——上游被掐断时走哪条取决于读协程当时停在哪里，是竞态。
func abruptAfterError(t *testing.T, withError bool) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("响应不支持 Hijack，这个夹具构造不出被掐断的连接")
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Errorf("Hijack: %v", err)
			return
		}
		defer conn.Close()
		writeTruncatedStream(buf, withError)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// writeTruncatedStream 声明一个远大于实际 body 的 Content-Length 再掐断，
// 让读侧确定地拿到 unexpected EOF 而不是一个干净的流结束。
func writeTruncatedStream(buf *bufio.ReadWriter, withError bool) {
	body := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","model":"k3"}}` + "\n\n"
	if withError {
		body += "event: error\n" +
			`data: {"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}` + "\n\n"
	}
	_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n" +
		"Content-Length: 100000\r\n\r\n" + body)
	_ = buf.Flush()
}

// errorThenSilence 发一个流内限流错误帧，然后不发终止符就正常返回。
// net/http 会干净收尾，于是读侧走的是 channel 关闭那条分支。
func errorThenSilence(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: error\n" +
			`data: {"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}` +
			"\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// 判据 16：上游不发终止符就干净返回时，流内错误的分类替代兜底。
//
// 这条走的是解码器补终止事件那条正常收尾路径（读协程的每条出口都带 done
// 或 err，所以「channel 关了却没收到 done」只可能是 ctx 掐断，那时归因在
// 预算/取消两步就返回了）。
func TestStreamErrorBeatsCleanChannelClose(t *testing.T) {
	f := newFixture(t, relaymock.Step{Target: target(errorThenSilence(t), "kimi-1/k3")})

	f.p.Serve(context.Background(), httptest.NewRecorder(), call(t, true))

	rec := f.col.record(t)
	if rec.ErrorCode != string(ir.ErrRateLimit) {
		t.Errorf("流水 error_code = %q，want %q——记成 upstream 会把限流"+
			"当成目标故障累计失败计数", rec.ErrorCode, ir.ErrRateLimit)
	}
	if contains([]byte(rec.ErrorMessage), "without a terminator") {
		t.Errorf("上游说过原因却报了兜底措辞：%q", rec.ErrorMessage)
	}
}

// 判据 17：scanner 报读错误那条分支上同样如此。
//
// 这条与判据 16 分开测而不是只留一条：两条分支各自调用 teardownCause，
// 只测一条的话另一处漏传 streamErr 在跑一次的测试里是绿的。
func TestStreamErrorBeatsTheReadError(t *testing.T) {
	f := newFixture(t, relaymock.Step{Target: target(abruptAfterError(t, true), "kimi-1/k3")})

	f.p.Serve(context.Background(), httptest.NewRecorder(), call(t, true))

	rec := f.col.record(t)
	if rec.ErrorCode != string(ir.ErrRateLimit) {
		t.Errorf("流水 error_code = %q，want %q", rec.ErrorCode, ir.ErrRateLimit)
	}
	if contains([]byte(rec.ErrorMessage), "stream broke") {
		t.Errorf("上游说过原因却报了传输层措辞：%q", rec.ErrorMessage)
	}
}

// 判据 18：上游没在流内说过话时仍报兜底错误。
//
// 这条守的是「加了优先级之后兜底归因没被丢掉」——只测优先的那一支，
// 把兜底整个删掉的改动也是绿的。
func TestSilentTeardownStillReportsTheTransportError(t *testing.T) {
	f := newFixture(t, relaymock.Step{Target: target(abruptAfterError(t, false), "kimi-1/k3")})

	f.p.Serve(context.Background(), httptest.NewRecorder(), call(t, true))

	rec := f.col.record(t)
	if rec.ErrorCode != string(ir.ErrUpstream) {
		t.Errorf("error_code = %q，want %q——没人说原因时兜底归因不能丢",
			rec.ErrorCode, ir.ErrUpstream)
	}
	if !contains([]byte(rec.ErrorMessage), "stream broke") {
		t.Errorf("兜底措辞丢了：%q", rec.ErrorMessage)
	}
}

// 判据 19：流内错误之后仍正常收尾时，行为与此前完全一致。
//
// 正常收尾路径读的是 streamErr 本身（bridge 的 done 分支），不经 teardownCause。
// 改了归因顺序不该动它。
func TestStreamErrorWithNormalTerminationIsUnchanged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeStream(w, "event: error\n"+
			`data: {"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`+
			"\n\n"+"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	t.Cleanup(srv.Close)
	f := newFixture(t, relaymock.Step{Target: target(srv.URL, "kimi-1/k3")})

	f.p.Serve(context.Background(), httptest.NewRecorder(), call(t, true))

	rec := f.col.record(t)
	if rec.ErrorCode != string(ir.ErrRateLimit) {
		t.Errorf("error_code = %q，want %q", rec.ErrorCode, ir.ErrRateLimit)
	}
}
