package pipeline_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/capture"
	"github.com/aceaura/model-surge-agent/backend/pipeline"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
	"github.com/aceaura/model-surge-agent/backend/relaymock"
)

// 三次尝试各自的身份与结果都能分辨：前两次分别落在不同账号上、各带自己的
// 错误码，第三次成功。这是这一轮的核心断言——在此之前流水只留最后一次。
func TestTrailDistinguishesEachAttempt(t *testing.T) {
	up := &fakeUpstream{handler: func(n int, w http.ResponseWriter) {
		switch n {
		case 0:
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
		case 1:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
		default:
			writeStream(w, okStream)
		}
	}}
	url := up.start(t)
	f := newFixture(t,
		relaymock.Step{Target: targetOn(url, "kimi-1/k3", "acc-a")},
		relaymock.Step{Target: targetOn(url, "ark-1/ds", "acc-b")},
		relaymock.Step{Target: targetOn(url, "kimi-2/k3", "acc-c")},
	)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, false))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}

	rec := f.col.record(t)
	trail := rec.AttemptsTrail
	if len(trail) != 3 {
		t.Fatalf("轨迹 = %d 项，要 3 项（三次尝试各一项）：%+v", len(trail), trail)
	}
	for i, it := range trail {
		if it.N != i+1 {
			t.Errorf("第 %d 项的 n = %d，序号必须从 1 起连续", i, it.N)
		}
	}
	wantAccounts := []string{"acc-a", "acc-b", "acc-c"}
	wantModels := []string{"kimi-1/k3", "ark-1/ds", "kimi-2/k3"}
	for i := range trail {
		if trail[i].Account != wantAccounts[i] {
			t.Errorf("第 %d 次尝试的 account = %q，要 %q", i+1, trail[i].Account, wantAccounts[i])
		}
		if trail[i].ModelID != wantModels[i] {
			t.Errorf("第 %d 次尝试的 model_id = %q，要 %q", i+1, trail[i].ModelID, wantModels[i])
		}
	}
	// 前两次各自的失败必须留在自己那一项里，而不是被最后一次覆盖。
	if trail[0].StatusCode != http.StatusTooManyRequests {
		t.Errorf("第一次的 status_code = %d，要 429", trail[0].StatusCode)
	}
	if !strings.Contains(trail[0].ErrorMessage, "slow down") {
		t.Errorf("第一次的错误原文丢了：%q", trail[0].ErrorMessage)
	}
	if !strings.Contains(trail[1].ErrorMessage, "boom") {
		t.Errorf("第二次的错误原文丢了：%q", trail[1].ErrorMessage)
	}
	if trail[0].ErrorCode == "" || trail[1].ErrorCode == "" {
		t.Errorf("失败的尝试没留错误码：%+v", trail[:2])
	}
	// 成功那次不带错误码：把它也填上会让「哪次真的失败了」无法按字段筛。
	if trail[2].ErrorCode != "" {
		t.Errorf("成功的尝试带了 error_code %q", trail[2].ErrorCode)
	}
	if trail[2].Outcome != relayclient.OutcomeNormal {
		t.Errorf("最后一次的 outcome = %q，要 normal", trail[2].Outcome)
	}
	if trail[2].StatusCode != http.StatusOK {
		t.Errorf("成功那次的 status_code = %d，要 200", trail[2].StatusCode)
	}
	// 中途的两次对调度层报的是 retrying，轨迹必须与上报同口径。
	for i := 0; i < 2; i++ {
		if trail[i].Outcome != relayclient.OutcomeRetrying {
			t.Errorf("第 %d 次的 outcome = %q，与上报给调度层的不一致", i+1, trail[i].Outcome)
		}
	}
}

// 不变式：轨迹各项耗时之和 == 行上的累计值，且每一项都是**本次**的量。
//
// 用注入的步进时钟而不是真实耗时：本机一次 mock 调用常常是零毫秒，
// 靠真耗时的和式会退化成 0 == 0 的空断言——那时「本次值写成累计值」
// 这种混用完全测不出来。
func TestTrailSegmentsSumToRowTotals(t *testing.T) {
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
	f.p.Now = newStepClock().now

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, false))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}

	rec := f.col.record(t)
	if len(rec.AttemptsTrail) != 2 {
		t.Fatalf("轨迹 = %d 项", len(rec.AttemptsTrail))
	}
	// 每次尝试各测一对时刻，因此每项恰好一个 step；累计值是两个 step。
	for i, it := range rec.AttemptsTrail {
		if it.DispatchMS != stepMS {
			t.Errorf("第 %d 项 dispatch_ms = %d，要 %d —— 它必须是本次尝试的量，"+
				"写成累计值会让「第三次才慢」读成「三次都慢」", i+1, it.DispatchMS, stepMS)
		}
		if it.UpstreamMS != stepMS {
			t.Errorf("第 %d 项 upstream_ms = %d，要 %d", i+1, it.UpstreamMS, stepMS)
		}
	}
	var dispatch, upstream int
	for _, it := range rec.AttemptsTrail {
		dispatch += it.DispatchMS
		upstream += it.UpstreamMS
	}
	if dispatch != rec.DispatchMS {
		t.Errorf("轨迹 dispatch 之和 = %d，行上累计 = %d —— "+
			"两者必须相等，否则两个计时器被混用了", dispatch, rec.DispatchMS)
	}
	if upstream != rec.UpstreamMS {
		t.Errorf("轨迹 upstream 之和 = %d，行上累计 = %d", upstream, rec.UpstreamMS)
	}
}

// 在建流之前就失败的那次尝试，本次上游耗时必须是 0 而不是上一次的值。
//
// 这是每次尝试开头清零唯一真正起作用的形态：本次值平时都由赋值覆盖，
// 而这条路径根本不会走到赋值点，不清零就会把上一次的上游耗时算到它头上，
// 于是轨迹之和也超出行上的累计值。
func TestTrailZeroesUpstreamForAttemptThatNeverOpened(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}}
	url := up.start(t)
	// 第二个目标的出站协议没有 codec：这一次在建流之前就失败。
	second := target(url, "ark-1/ds")
	second.Protocol = "no-such-protocol"
	f := newFixture(t,
		relaymock.Step{Target: target(url, "kimi-1/k3")},
		relaymock.Step{Target: second},
	)
	f.p.Now = newStepClock().now
	// 两次就收：再让它试第三次会多出一项「调度层没步骤了」的轨迹，
	// 与这条用例要看的东西无关。
	f.p.Opts.MaxAttempts = 2

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, false))
	if w.Code == http.StatusOK {
		t.Fatalf("这条本该失败：%s", w.Body)
	}

	rec := f.col.record(t)
	if len(rec.AttemptsTrail) != 2 {
		t.Fatalf("轨迹 = %d 项：%+v", len(rec.AttemptsTrail), rec.AttemptsTrail)
	}
	if rec.AttemptsTrail[0].UpstreamMS != stepMS {
		t.Errorf("第一次的 upstream_ms = %d，要 %d", rec.AttemptsTrail[0].UpstreamMS, stepMS)
	}
	if got := rec.AttemptsTrail[1].UpstreamMS; got != 0 {
		t.Errorf("第二次根本没连上游，upstream_ms 却是 %d —— "+
			"那是上一次的值被带了过来", got)
	}
	var upstream int
	for _, it := range rec.AttemptsTrail {
		upstream += it.UpstreamMS
	}
	if upstream != rec.UpstreamMS {
		t.Errorf("轨迹 upstream 之和 = %d，行上累计 = %d", upstream, rec.UpstreamMS)
	}
}

// 没装上报实现时轨迹照样要留。
//
// 轨迹的追加与向调度层上报同在 report 里，一旦排到 Reporter 判空之后，
// 「上报没配」会静默连带把本地诊断也关掉——而这两件事毫无关系。
func TestTrailSurvivesWithoutReporter(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})
	f.p.Reporter = nil

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, false))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}

	rec := f.col.record(t)
	if len(rec.AttemptsTrail) != 1 {
		t.Fatalf("没装上报实现时轨迹丢了：%+v", rec.AttemptsTrail)
	}
	if rec.AttemptsTrail[0].ModelID != "kimi-1/k3" {
		t.Errorf("轨迹项 = %+v", rec.AttemptsTrail[0])
	}
}

// 调度层没给出目标的那次尝试也要留痕，且目标三项必须留空。
//
// 填成空字符串以外的任何东西都会让「哪个账号总失败」的统计算进一个
// 不存在的账号。
func TestTrailKeepsDispatchFailureWithoutTarget(t *testing.T) {
	f := newFixture(t, relaymock.Step{
		Err: &relayclient.Error{
			Code: relayclient.CodeTargetUnavailable, Message: "no target left"},
	})

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, false))
	if w.Code == http.StatusOK {
		t.Fatalf("这条本该失败：%s", w.Body)
	}

	rec := f.col.record(t)
	if len(rec.AttemptsTrail) != 1 {
		t.Fatalf("轨迹 = %d 项，调度失败那次也要留痕：%+v", len(rec.AttemptsTrail), rec.AttemptsTrail)
	}
	it := rec.AttemptsTrail[0]
	if it.N != 1 {
		t.Errorf("n = %d", it.N)
	}
	if it.ModelID != "" || it.Account != "" || it.OutboundProtocol != "" {
		t.Errorf("调度层没给目标，轨迹却填了目标：%+v", it)
	}
	if it.ErrorCode == "" {
		t.Errorf("调度失败没留错误码：%+v", it)
	}
	if it.Outcome != relayclient.OutcomeAbnormal {
		t.Errorf("outcome = %q，要 abnormal", it.Outcome)
	}
}

// 提交后失败（流发了一半再断）也要留在轨迹里，且这一项记的是失败。
func TestTrailRecordsCommittedFailure(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
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

	f.p.Serve(context.Background(), httptest.NewRecorder(), call(t, true))

	rec := f.col.record(t)
	if len(rec.AttemptsTrail) != 1 {
		t.Fatalf("轨迹 = %d 项", len(rec.AttemptsTrail))
	}
	if rec.AttemptsTrail[0].ErrorCode == "" {
		t.Errorf("提交后失败在轨迹里没留错误码：%+v", rec.AttemptsTrail[0])
	}
}

// 不可重试的失败只留一项：它不该被当成「还能换一个」而多记一次。
func TestTrailStopsAtNonRetryableFailure(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"bad field"}}`))
	}}
	url := up.start(t)
	f := newFixture(t,
		relaymock.Step{Target: target(url, "kimi-1/k3")},
		relaymock.Step{Target: target(url, "ark-1/ds")},
	)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, false))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if up.calls() != 1 {
		t.Fatalf("上游被打了 %d 次，400 不该换目标重试", up.calls())
	}

	rec := f.col.record(t)
	if len(rec.AttemptsTrail) != 1 {
		t.Fatalf("轨迹 = %d 项，不可重试只该有一项：%+v", len(rec.AttemptsTrail), rec.AttemptsTrail)
	}
	if rec.AttemptsTrail[0].StatusCode != http.StatusBadRequest {
		t.Errorf("status_code = %d，要 400", rec.AttemptsTrail[0].StatusCode)
	}
}

// 轨迹项里绝不能出现凭据、BaseURL 或请求体。
//
// target.Headers 只在内存里活着，一旦被顺手拷进轨迹就会随流水进 PG。
func TestTrailNeverCarriesCredentialsOrBodies(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})

	f.p.Serve(context.Background(), httptest.NewRecorder(), call(t, false))

	rec := f.col.record(t)
	raw, err := json.Marshal(rec.AttemptsTrail)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, leak := range []string{"sk-secret-1234", "x-api-key", url, "127.0.0.1", "messages"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("轨迹里泄了 %q：%s", leak, raw)
		}
	}
}

// n 与两段耗时都不带 omitempty：零值要照样出现在 JSON 里。
//
// n=0 是 bug 信号，耗时 0 是「快到不足一毫秒」这个有意义的观测值；
// 让它们消失会把 bug 与观测一起藏掉。
func TestTrailKeysSurviveZeroValues(t *testing.T) {
	raw, err := json.Marshal(pipeline.AttemptRecord{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"n":0`, `"dispatch_ms":0`, `"upstream_ms":0`, `"outcome":""`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("零值时 %s 被省掉了：%s", key, raw)
		}
	}
	// 反过来，可选项在零值时必须省掉，否则读的人会把空目标当成有目标。
	for _, key := range []string{"model_id", "account", "error_code", "retry_after"} {
		if strings.Contains(string(raw), key) {
			t.Errorf("零值时 %q 不该出现：%s", key, raw)
		}
	}
}

// 捕获的上游字节按尝试可分：单次尝试没有标记，多次尝试之间有。
//
// 这是上一轮留下的缺口——两次尝试的上游响应首尾相接，读的人分不出
// 「先回了 429 再回了正文」与「一次就回了这些」。
func TestCaptureMarksAttemptBoundaries(t *testing.T) {
	up := &fakeUpstream{handler: func(n int, w http.ResponseWriter) {
		if n == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"first boom"}}`))
			return
		}
		writeStream(w, okStream)
	}}
	url := up.start(t)
	f := newFixture(t,
		relaymock.Step{Target: target(url, "kimi-1/k3")},
		relaymock.Step{Target: target(url, "ark-1/ds")},
	)
	st := capture.New(capture.Options{Mode: capture.ModeAll})
	c := call(t, false)
	c.Capture = st.Begin(c.RequestID)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}

	got := string(snapshotOf(t, st, c.RequestID).Bodies[capture.UpstreamResponse].Bytes)
	if strings.Contains(got, "attempt 1") {
		t.Errorf("第一次尝试前面没有要分开的东西，不该有标记：%q", got)
	}
	if !strings.Contains(got, "attempt 2") {
		t.Errorf("两次尝试的上游字节之间没有边界标记，读的人分不开：%q", got)
	}
	// 标记必须落在两段之间，而不是被追加到末尾。
	boom := strings.Index(got, "first boom")
	mark := strings.Index(got, "attempt 2")
	start := strings.Index(got, "message_start")
	if boom < 0 || mark < 0 || start < 0 || !(boom < mark && mark < start) {
		t.Errorf("边界标记不在两段之间（boom=%d mark=%d start=%d）：%q",
			boom, mark, start, got)
	}
}

// 单次尝试的捕获里完全没有标记：只有一段就没有边界。
func TestCaptureHasNoMarkerForSingleAttempt(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})
	st := capture.New(capture.Options{Mode: capture.ModeAll})
	c := call(t, false)
	c.Capture = st.Begin(c.RequestID)

	f.p.Serve(context.Background(), httptest.NewRecorder(), c)

	got := string(snapshotOf(t, st, c.RequestID).Bodies[capture.UpstreamResponse].Bytes)
	if strings.Contains(got, "attempt") {
		t.Errorf("单次尝试的捕获里出现了边界标记：%q", got)
	}
}

// targetOn 与 target 同，另外能改 account，用来区分各次尝试落在哪个账号。
func targetOn(baseURL, modelID, account string) relayclient.Target {
	tg := target(baseURL, modelID)
	tg.Account = account
	return tg
}
