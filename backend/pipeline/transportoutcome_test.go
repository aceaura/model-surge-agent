package pipeline_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
	"github.com/aceaura/model-surge-agent/backend/relaymock"
)

// 本文件守连接层故障的归因链路：从 open 的 Do 失败一路到上报的 outcome。
//
// 这一段是纯转写，最容易在改动里静默断掉，而断掉的症状是「一条坏连接把
// 健康账号推向冷却」——运维只会看到账号莫名冷却，看不出根因在这里。

// deadBaseURL 返回一个没人监听的地址。
//
// 连接被拒是最稳定的连接层故障形态：不依赖时序、不依赖 h2，
// 在所有平台上都是 ECONNREFUSED。
func deadBaseURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	// 立刻关掉：这个端口刚才可用、现在没人听，正是死连接的效果。
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return "http://" + addr
}

func TestConnectionFailureReportsTransportOutcome(t *testing.T) {
	f := newFixture(t,
		relaymock.Step{Target: target(deadBaseURL(t), "kimi-1/k3")},
		relaymock.Step{Target: target(deadBaseURL(t), "kimi-2/k3")},
		relaymock.Step{Target: target(deadBaseURL(t), "kimi-3/k3")},
	)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	for _, got := range f.col.outcomes() {
		if got != relayclient.OutcomeTransport {
			t.Fatalf("outcome = %q，连接层故障必须单独上报："+
				"记成 retrying/abnormal 会让一条坏连接累计到账号的失败计数上",
				got)
		}
	}
	if len(f.col.outcomes()) == 0 {
		t.Fatal("一次都没上报，归因链路根本没跑到")
	}
}

func TestConnectionFailureIsRetryableAcrossTargets(t *testing.T) {
	// 第一个目标连不上，第二个正常：必须换过去并成功。
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	good := up.start(t)
	f := newFixture(t,
		relaymock.Step{Target: target(deadBaseURL(t), "kimi-1/k3")},
		relaymock.Step{Target: target(good, "kimi-2/k3")},
	)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，连接层故障必须可重试——坏的是连接不是目标", w.Code)
	}
	outcomes := f.col.outcomes()
	if len(outcomes) != 2 {
		t.Fatalf("上报 %d 次，要 2 次（一次连接故障 + 一次成功）: %v", len(outcomes), outcomes)
	}
	if outcomes[0] != relayclient.OutcomeTransport {
		t.Errorf("第一次 outcome = %q，要 %q", outcomes[0], relayclient.OutcomeTransport)
	}
	if outcomes[1] != relayclient.OutcomeNormal {
		t.Errorf("第二次 outcome = %q，要 %q", outcomes[1], relayclient.OutcomeNormal)
	}
}

// 流水里要记成 transport 而不是 upstream：这两类的排查方向完全不同，
// 一个查我们的连接池、一个查上游。
func TestConnectionFailureRecordsTransportKind(t *testing.T) {
	// 三个目标全连不上，刚好用完 MaxAttempts。
	//
	// 只给一个目标的话，第二轮 Dispatch 会因候选耗尽先失败，
	// 流水里留下的就是那个 not_found——测到的是候选耗尽而不是连接层归因。
	f := newFixture(t,
		relaymock.Step{Target: target(deadBaseURL(t), "kimi-1/k3")},
		relaymock.Step{Target: target(deadBaseURL(t), "kimi-2/k3")},
		relaymock.Step{Target: target(deadBaseURL(t), "kimi-3/k3")},
	)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	rec := f.col.record(t)
	if rec.ErrorCode != string(ir.ErrTransport) {
		t.Fatalf("流水 error_code = %q，要 %q——"+
			"记成 upstream 会把排查引向上游，而问题在我们的连接池",
			rec.ErrorCode, ir.ErrTransport)
	}
}

// 上游有响应（哪怕是 5xx）就不是连接层：连上了就说明连接是好的。
func TestUpstreamErrorIsNotTransport(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"type":"overloaded_error","message":"busy"}}`))
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	for i, got := range f.col.outcomes() {
		if got == relayclient.OutcomeTransport {
			t.Fatalf("第 %d 次 outcome = %q：上游回了 503，"+
				"连接是好的，归成连接层会让真实的上游故障不再累计失败、永不冷却",
				i, got)
		}
	}
}

// 502 而不是 500：客户端据此能分清「没连上上游」与「上游出错了」。
func TestTransportKindMapsTo502(t *testing.T) {
	if got := codec.StatusForKind(ir.ErrTransport); got != http.StatusBadGateway {
		t.Fatalf("StatusForKind(transport) = %d，要 502", got)
	}
	if codec.StatusForKind(ir.ErrUpstream) == http.StatusBadGateway {
		t.Fatal("upstream 也映射成 502 了，两类就分不开了")
	}
}

func TestTransportKindIsRetryable(t *testing.T) {
	if !ir.NewError(ir.ErrTransport, 0, "", "x").Retryable {
		t.Fatal("transport 必须可重试：坏的是连接不是目标，换一个一定可以重来")
	}
}
