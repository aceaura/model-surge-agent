package pipeline_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/capture"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
	"github.com/aceaura/model-surge-agent/backend/relaymock"
)

func snapshotOf(t *testing.T, st *capture.Store, requestID string) *capture.Snapshot {
	t.Helper()
	snap, ok := st.Get(requestID)
	if !ok {
		t.Fatalf("捕获 %q 不存在", requestID)
	}
	return snap
}

// 换目标重试时出站 wire body 覆盖、上游字节累加。
//
// 两种语义刻意不同：出站 body 的诊断对象是「最终发出去的那一次」，
// 累加会把一份没被采用的 body 拼在前面；而上游字节是连续的流片段，
// 两次尝试各自的响应都要留，否则看不出第一次是怎么坏的。
func TestCaptureOverwritesOutboundAndAccumulatesUpstreamAcrossRetries(t *testing.T) {
	up := &fakeUpstream{handler: func(n int, w http.ResponseWriter) {
		if n == 0 {
			// 首次回 5xx：提交前失败，可换目标。
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"first_attempt boom"}}`))
			return
		}
		writeStream(w, okStream)
	}}
	url := up.start(t)
	f := newFixture(t,
		relaymock.Step{Target: target(url, "kimi-1/k3")},
		relaymock.Step{Target: targetNamed(url, "ark-1/ds", "ark-native")},
	)

	st := capture.New(capture.Options{Mode: capture.ModeAll})
	c := call(t, false)
	c.Capture = st.Begin(c.RequestID)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if up.calls() != 2 {
		t.Fatalf("上游被打了 %d 次，这条用例要的是换目标重试", up.calls())
	}

	snap := snapshotOf(t, st, c.RequestID)
	upReq := string(snap.Bodies[capture.UpstreamRequest].Bytes)
	upResp := string(snap.Bodies[capture.UpstreamResponse].Bytes)

	// 覆盖：只剩第二个目标的 native model。
	if !strings.Contains(upReq, "ark-native") {
		t.Errorf("upstream_request 不是最后一次发出的那份：%q", upReq)
	}
	if strings.Contains(upReq, "kimi-k3-256k") {
		t.Errorf("upstream_request 里还留着第一次的 native model，"+
			"累加会让读的人分不开两份：%q", upReq)
	}
	// 累加：两次尝试的上游字节都在。
	// 5xx 的错误体也必须留：DecodeError 会把它归一化成一个 ir.Error，
	// 归错的时候只有原文能说明上游到底说了什么。
	if !strings.Contains(upResp, "first_attempt boom") {
		t.Errorf("第一次尝试的上游错误体丢了，看不出它是怎么坏的：%q", upResp)
	}
	if !strings.Contains(upResp, "message_start") {
		t.Errorf("第二次尝试的上游字节丢了：%q", upResp)
	}
}

// 上游连接就失败时捕获里必须已有出站 body：那正是要看的东西——
// 发出去的是什么。捕获点在 open 之前就是为了这个形态。
func TestCaptureHoldsOutboundBodyWhenUpstreamNeverAnswers(t *testing.T) {
	f := newFixture(t, relaymock.Step{
		// 指向一个不存在的地址：连接层失败，没有任何上游字节。
		Target: target("http://127.0.0.1:1", "kimi-1/k3"),
	})
	st := capture.New(capture.Options{Mode: capture.ModeErrors})
	c := call(t, false)
	c.Capture = st.Begin(c.RequestID)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, c)
	if w.Code == http.StatusOK {
		t.Fatalf("这条本该失败：%s", w.Body)
	}

	snap := snapshotOf(t, st, c.RequestID)
	if got := string(snap.Bodies[capture.UpstreamRequest].Bytes); !strings.Contains(got, "kimi-k3-256k") {
		t.Errorf("连接失败时出站 body 没被捕获，而那正是要看的：%q", got)
	}
	if n := len(snap.Bodies[capture.UpstreamResponse].Bytes); n != 0 {
		t.Errorf("上游一个字节都没回，捕获里却有 %d 字节", n)
	}
	// 错误信封是回给客户端的字节。
	if got := string(snap.Bodies[capture.ClientResponse].Bytes); !strings.Contains(got, `"error"`) {
		t.Errorf("错误信封没被捕获：%q", got)
	}
}

// errors 档的判据是 rec.ErrorCode 非空，不是 outcome：
// 换目标重试成功的那条整体是正常的，不该被留下。
func TestErrorsModeDiscardsRequestThatSucceededAfterRetry(t *testing.T) {
	up := &fakeUpstream{handler: func(n int, w http.ResponseWriter) {
		if n == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
			return
		}
		writeStream(w, okStream)
	}}
	url := up.start(t)
	f := newFixture(t,
		relaymock.Step{Target: target(url, "kimi-1/k3")},
		relaymock.Step{Target: target(url, "ark-1/ds")},
	)
	st := capture.New(capture.Options{Mode: capture.ModeErrors})
	c := call(t, false)
	c.Capture = st.Begin(c.RequestID)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, c)
	if w.Code != http.StatusOK {
		t.Fatalf("重试后应成功，status = %d: %s", w.Code, w.Body)
	}
	if got := len(st.List()); got != 0 {
		t.Errorf("重试成功的请求被当成失败留下了 %d 条 —— "+
			"判据是 ErrorCode 非空，中途 retrying 不算失败", got)
	}
}

// 提交后失败（流已发出一半再断）必须留下：这是最需要看原始字节的形态，
// 客户端已经收到半个回答，而问题可能在上游也可能在转换。
func TestErrorsModeKeepsCommittedFailure(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		// 发到工具入参一半就断：聚合器会判定不可安全闭合。
		writeStream(w, `event: message_start
data: {"type":"message_start","message":{"id":"msg_1"}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"f","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}

`)
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})
	st := capture.New(capture.Options{Mode: capture.ModeErrors})
	c := call(t, true)
	c.Capture = st.Begin(c.RequestID)

	f.p.Serve(context.Background(), httptest.NewRecorder(), c)

	snap := snapshotOf(t, st, c.RequestID)
	if !strings.Contains(string(snap.Bodies[capture.UpstreamResponse].Bytes), "input_json_delta") {
		t.Errorf("提交后失败的上游字节没留下：%q", snap.Bodies[capture.UpstreamResponse].Bytes)
	}
}

// 客户端取消是两个判据唯一分歧的形态：outcome 记 normal（客户端自己走了
// 不是目标的故障，不该累计它的失败计数），而 error_code 是 canceled。
// 按 outcome 判会把取消的请求丢掉，而「客户端为什么取消」往往正是要看
// 上游当时发了什么才能回答的。
func TestErrorsModeKeepsClientCancelWhereOutcomeSaysNormal(t *testing.T) {
	canceled := make(chan struct{})
	up := streamThenHang(t, canceled)
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})

	st := capture.New(capture.Options{Mode: capture.ModeErrors})
	c := call(t, true)
	c.Capture = st.Begin(c.RequestID)

	ctx, cancel := context.WithCancel(context.Background())
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.p.Serve(ctx, w, c)
	}()
	waitFor(t, func() bool { return w.Body.Len() > 0 })
	cancel()
	<-done
	close(canceled)

	if len(f.col.records) != 1 {
		t.Fatalf("流水 = %d 条", len(f.col.records))
	}
	rec := f.col.records[0]
	if rec.Outcome != relayclient.OutcomeNormal {
		t.Fatalf("outcome = %q，这条用例要的是「outcome 正常但 error_code 非空」", rec.Outcome)
	}
	if rec.ErrorCode == "" {
		t.Fatal("error_code 为空，取消没被记下来")
	}
	snap := snapshotOf(t, st, c.RequestID)
	if !strings.Contains(string(snap.Bodies[capture.UpstreamResponse].Bytes), "partial") {
		t.Errorf("取消时上游已发的字节没留下：%q", snap.Bodies[capture.UpstreamResponse].Bytes)
	}
}

// targetNamed 与 target 同，另外能改 native model，
// 用来区分两次尝试各自的出站 body。
func targetNamed(baseURL, modelID, native string) relayclient.Target {
	tg := target(baseURL, modelID)
	tg.NativeModel = native
	return tg
}
