package pipeline_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/relayclient"
	"github.com/aceaura/model-surge-agent/backend/relaymock"
)

// 本文件守的是「客户端自己走了」与「上游真的故障了」不能混为一谈。
//
// 混在一起的后果是运维侧的：取消记成 abnormal 会累计目标的失败计数，
// 客户端多按几次停止就能把一个健康账号推向冷却。

// streamThenHang 先发一段内容让本轮 committed，再挂住不发终止帧，
// 留出取消的时间窗。
func streamThenHang(t *testing.T, canceled <-chan struct{}) *fakeUpstream {
	t.Helper()
	return &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, `event: message_start
data: {"type":"message_start","message":{"id":"msg_1"}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}

`)
		// 等到取消发生，模拟上游还在慢慢生成。
		select {
		case <-canceled:
		case <-time.After(3 * time.Second):
		}
	}}
}

// 流式客户端取消：记 normal 不累计失败，error_code 要能与上游故障区分。
func TestStreamingClientCancelIsNotTheTargetsFault(t *testing.T) {
	canceled := make(chan struct{})
	up := streamThenHang(t, canceled)
	url := up.start(t)
	f := newFixture(t,
		relaymock.Step{Target: target(url, "kimi-1/k3")},
		relaymock.Step{Target: target(url, "ark-1/ds")},
	)

	ctx, cancel := context.WithCancel(context.Background())
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.p.Serve(ctx, w, call(t, true))
	}()

	// 等首帧写出（已 committed）再取消，否则测到的是「提交前失败换目标」。
	waitFor(t, func() bool { return w.Body.Len() > 0 })
	cancel()
	close(canceled)
	<-done

	assertCanceled(t, f, up)
}

// 非流式客户端取消。这一例是本轮的核心回归：
// 非流式请求在聚合完成前一个字节都不写客户端，所以「写客户端失败」那条
// 分支永远不会触发，取消只能从 ctx 观察到。改动前它必然被记成上游故障。
func TestNonStreamingClientCancelIsNotTheTargetsFault(t *testing.T) {
	canceled := make(chan struct{})
	up := streamThenHang(t, canceled)
	url := up.start(t)
	f := newFixture(t,
		relaymock.Step{Target: target(url, "kimi-1/k3")},
		relaymock.Step{Target: target(url, "ark-1/ds")},
	)

	ctx, cancel := context.WithCancel(context.Background())
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.p.Serve(ctx, w, call(t, false))
	}()

	// 非流式看不到写出的字节，等上游被打到即可——此时首帧已在解码路上。
	waitFor(t, func() bool { return up.calls() == 1 })
	time.Sleep(100 * time.Millisecond)
	cancel()
	close(canceled)
	<-done

	assertCanceled(t, f, up)
}

func assertCanceled(t *testing.T, f *fixture, up *fakeUpstream) {
	t.Helper()
	if got := up.calls(); got != 1 {
		t.Errorf("upstream calls = %d，客户端已经不要这个回答了，不该换目标重试", got)
	}
	got := f.col.outcomes()
	if len(got) != 1 || got[0] != relayclient.OutcomeNormal {
		t.Errorf("outcomes = %v，客户端取消不是目标的失败，必须记 normal", got)
	}
	rec := f.col.record(t)
	if rec.ErrorCode != "canceled" {
		t.Errorf("error_code = %q，取消必须与上游故障可区分（upstream）", rec.ErrorCode)
	}
}

// ctx 没取消而上游真的断流：仍要按上游故障处理，判定不得把真实故障吞掉。
func TestUpstreamBreakWithoutCancelIsStillTheTargetsFault(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		// 发完首帧就断，不给终止帧，且客户端没有取消。
		writeStream(w, `event: message_start
data: {"type":"message_start","message":{"id":"msg_1"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"cut"}}

`)
	}}
	url := up.start(t)
	f := newFixture(t,
		relaymock.Step{Target: target(url, "kimi-1/k3")},
		relaymock.Step{Target: target(url, "ark-1/ds")},
	)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	rec := f.col.record(t)
	if rec.ErrorCode == "canceled" {
		t.Error("上游断流被误判成客户端取消，真实故障会被吞掉")
	}
}

// 取消发生在 committed 之前：同样不换目标。
// 换目标是为了绕开坏账号，而这里账号没问题，是客户端走了。
func TestCancelBeforeCommitDoesNotSwapTargets(t *testing.T) {
	release := make(chan struct{})
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		// 一帧都不发，等取消。
		select {
		case <-release:
		case <-time.After(3 * time.Second):
		}
	}}
	url := up.start(t)
	f := newFixture(t,
		relaymock.Step{Target: target(url, "kimi-1/k3")},
		relaymock.Step{Target: target(url, "ark-1/ds")},
	)

	ctx, cancel := context.WithCancel(context.Background())
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.p.Serve(ctx, w, call(t, true))
	}()

	waitFor(t, func() bool { return up.calls() == 1 })
	cancel()
	close(release)
	<-done

	if got := up.calls(); got != 1 {
		t.Errorf("upstream calls = %d，取消之后不该继续试别的目标", got)
	}
}

// waitFor 轮询等条件成立，超时即失败。
// 用轮询而非固定 sleep：固定时长在慢机器上会假失败，在快机器上纯浪费。
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("等待条件成立超时")
}
