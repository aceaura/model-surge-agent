package pipeline_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
	"github.com/aceaura/model-surge-agent/backend/relaymock"
)

// 本文件守累积闸门在数据面的处置。判据 12–15。
//
// 闸门本身在 ir 包里测（ir/overflow_test.go），这里测的是撞线之后这一趟请求
// 怎么收尾：谁拿到什么形状的错误、算不算目标的失败、要不要重试。

// oversizedStream 发一个块数远超上限的流。
//
// 用块数闸门而不是字节闸门来触发：后者要真的推 32 MiB 过 loopback，而这里
// 要断言的是撞线之后的处置，与撞的是哪道闸门无关。
func oversizedStream(t *testing.T, blocks int) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","model":"kimi-k3-256k"}}` +
		"\n\n")
	for i := 0; i < blocks; i++ {
		fmt.Fprintf(&b, "event: content_block_start\n"+
			`data: {"type":"content_block_start","index":%d,`+
			`"content_block":{"type":"text","text":"x"}}`+"\n\n", i)
	}
	b.WriteString("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	raw := b.String()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeStream(w, raw)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// 判据 12：流式客户端撞线时以流内错误帧收尾。
//
// 状态码在首帧就定了（200），收不回来；补一个正常终止帧会把残缺内容伪装成
// 完整回答，客户端会把它存进历史。
func TestOverflowOnStreamEndsWithInStreamError(t *testing.T) {
	f := newFixture(t, relaymock.Step{Target: target(oversizedStream(t, 4100), "kimi-1/k3")})
	w := httptest.NewRecorder()

	f.p.Serve(context.Background(), w, call(t, true))

	if w.Code != http.StatusOK {
		t.Errorf("流式状态码 = %d，want 200——首帧已提交，状态码不可再改", w.Code)
	}
	if !contains(w.Body.Bytes(), "event: error") {
		t.Errorf("响应里没有流内错误帧，客户端会把残缺内容当完整回答：\n%s",
			tailOf(w.Body.String()))
	}
	rec := f.col.record(t)
	if rec.ErrorCode == "" {
		t.Error("流水 error_code 为空——这是一条查不出原因的流水")
	}
	if !strings.Contains(rec.ErrorMessage, "content block limit") {
		t.Errorf("流水 error_message = %q，里面没说撞的是哪道闸门", rec.ErrorMessage)
	}
}

// 判据 13：非流式客户端撞线时拿到 HTTP 错误信封。
//
// 它一个字节都还没收到，所以状态码仍能给对；交半份聚合结果等于把残缺响应
// 伪装成完整的。
func TestOverflowOnNonStreamReturnsErrorEnvelope(t *testing.T) {
	f := newFixture(t, relaymock.Step{Target: target(oversizedStream(t, 4100), "kimi-1/k3")})
	w := httptest.NewRecorder()

	f.p.Serve(context.Background(), w, call(t, false))

	if w.Code == http.StatusOK {
		t.Errorf("非流式撞线仍回 200，body：\n%s", tailOf(w.Body.String()))
	}
	if !contains(w.Body.Bytes(), "content block limit") {
		t.Errorf("错误体里没说撞的是哪道闸门：\n%s", tailOf(w.Body.String()))
	}
}

// 判据 14：不重试。
//
// 换个目标重新生成一遍还是同样的规模，而重试会把这笔代价再付一次；
// 标成可重试等于让一个超大响应连着打满整个目标组。
func TestOverflowDoesNotRetry(t *testing.T) {
	f := newFixture(t,
		relaymock.Step{Target: target(oversizedStream(t, 4100), "kimi-1/k3")},
		relaymock.Step{Target: target(oversizedStream(t, 4100), "kimi-2/k3")},
		relaymock.Step{Target: target(oversizedStream(t, 4100), "kimi-1/k3")},
	)

	f.p.Serve(context.Background(), httptest.NewRecorder(), call(t, true))

	if n := len(f.relay.Dispatches()); n != 1 {
		t.Errorf("派发了 %d 次，want 1——撞线不可重试", n)
	}
	for _, o := range f.col.outcomes() {
		if o == "retrying" {
			t.Errorf("上报里出现 retrying：%v", f.col.outcomes())
		}
	}
}

// 判据 15：归因是上游而不是本服务或客户端。
//
// 内容是上游发来的，本服务只是拒绝无界地存它。记成 transport 或 canceled
// 会让运维去查网络或客户端，而真相是这个目标回了一份超规模响应。
func TestOverflowIsAttributedToUpstream(t *testing.T) {
	f := newFixture(t, relaymock.Step{Target: target(oversizedStream(t, 4100), "kimi-1/k3")})

	f.p.Serve(context.Background(), httptest.NewRecorder(), call(t, true))

	rec := f.col.record(t)
	if rec.ErrorCode != string(ir.ErrUpstream) {
		t.Errorf("error_code = %q，want %q", rec.ErrorCode, ir.ErrUpstream)
	}
	outs := f.col.outcomes()
	if len(outs) == 0 || outs[len(outs)-1] != "abnormal" {
		t.Errorf("终态上报 = %v，want 末项 abnormal", outs)
	}
}

// 判据 12 续：正常规模的流式请求不受影响。
//
// 闸门对绝大多数请求必须是恒等的。只测触发那一侧的话，把闸门改成恒触发
// 也是绿的。
func TestOrdinaryStreamIsUnaffectedByGates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeStream(w, okStream)
	}))
	t.Cleanup(srv.Close)
	f := newFixture(t, relaymock.Step{Target: target(srv.URL, "kimi-1/k3")})
	w := httptest.NewRecorder()

	f.p.Serve(context.Background(), w, call(t, true))

	rec := f.col.record(t)
	if rec.ErrorCode != "" {
		t.Errorf("普通流式请求报了错：%q / %q", rec.ErrorCode, rec.ErrorMessage)
	}
	if !contains(w.Body.Bytes(), "hello") {
		t.Errorf("正文没发出去：\n%s", tailOf(w.Body.String()))
	}
}

// tailOf 只取响应尾部用于报错，避免把一份超大 body 全打进测试输出。
func tailOf(s string) string {
	const n = 400
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
