package pipeline_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/relayclient"
	"github.com/aceaura/model-surge-agent/backend/relaymock"
)

// 四个终态分支都要设 usage。用性质遍历而不是四个独立用例：
// 独立用例在将来加第五个分支时不会失败。
//
// 判据是「流水里的 usage 与上报给调度层的 usage 一致」——这正是对账时
// 两边对不上的那个量。
func TestEveryTerminalBranchRecordsUsage(t *testing.T) {
	cases := []struct {
		name string
		// handler 决定假上游怎么回应，据此选中一条终态分支。
		handler func(n int, w http.ResponseWriter)
		stream  bool
		// estimate 打开字符数估算兜底，让上游不报 usage 时仍有账可记。
		estimate bool
		// wantUsage 要求这条路径的 usage 非零：全零时对账断言恒成立。
		wantUsage bool
	}{
		{
			name: "normal",
			handler: func(_ int, w http.ResponseWriter) {
				writeStream(w, okStream)
			},
			stream: true,
		},
		{
			name: "committed-then-failed",
			handler: func(_ int, w http.ResponseWriter) {
				// 发出内容后以流内错误收尾：committed 分支。
				writeStream(w, `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":11}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}

event: error
data: {"type":"error","error":{"type":"api_error","message":"upstream exploded"}}

`)
			},
			stream: true,
		},
		{
			name: "non-retryable",
			handler: func(_ int, w http.ResponseWriter) {
				// 400 不可重试：直接回错那条分支。
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"bad"}}`))
			},
			stream: true,
		},
		{
			name: "retries-exhausted",
			handler: func(_ int, w http.ResponseWriter) {
				// 502 可重试，每次都失败：耗尽那条分支。
				w.WriteHeader(http.StatusBadGateway)
			},
			stream: true,
		},
		{
			// 耗尽分支还要有一个 usage 非零的形态：502 那条两边都是零，
			// 对账断言在全零时恒成立，等于没测。200 空流也判可重试，
			// 而它会走到 finish 的估算兜底，于是耗尽时手里确实有账要记。
			name: "retries-exhausted-with-usage",
			handler: func(_ int, w http.ResponseWriter) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
			},
			stream:    true,
			estimate:  true,
			wantUsage: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up := &fakeUpstream{handler: c.handler}
			url := up.start(t)
			f := newFixture(t,
				relaymock.Step{Target: target(url, "m1")},
				relaymock.Step{Target: target(url, "m2")},
				relaymock.Step{Target: target(url, "m3")},
			)
			f.p.Opts.EstimateUsage = c.estimate

			f.p.Serve(context.Background(), httptest.NewRecorder(), call(t, c.stream))

			rec := f.col.record(t)
			f.col.mu.Lock()
			reports := append([]relayclient.ResultReport(nil), f.col.reports...)
			f.col.mu.Unlock()
			if len(reports) == 0 {
				t.Fatal("一条上报都没有")
			}
			last := reports[len(reports)-1]
			if !reflect.DeepEqual(rec.Usage, last.Usage) {
				t.Errorf("流水 usage = %+v，上报 usage = %+v：两边对不上账",
					rec.Usage, last.Usage)
			}
			if c.wantUsage && rec.Usage.InputTokens == 0 && rec.Usage.OutputTokens == 0 {
				t.Errorf("流水 usage 全零：这条路径本该有账可记，对账断言等于没测")
			}
		})
	}
}

// 上报顺序不得因为异步化而乱：调度层按到达顺序解释它们
// （retrying 之后才是终态）。
func TestReportOrderPreservedAcrossRetries(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadGateway)
	}}
	url := up.start(t)
	f := newFixture(t,
		relaymock.Step{Target: target(url, "m1")},
		relaymock.Step{Target: target(url, "m2")},
		relaymock.Step{Target: target(url, "m3")},
	)

	f.p.Serve(context.Background(), httptest.NewRecorder(), call(t, true))

	want := []string{relayclient.OutcomeRetrying, relayclient.OutcomeRetrying,
		relayclient.OutcomeAbnormal}
	if got := f.col.outcomes(); !equal(got, want) {
		t.Errorf("上报顺序 = %v, want %v", got, want)
	}
}

// Serve 返回时这次请求的全部上报都已投出：异步化不得把它们留在在途。
// 不等的话进程关停会丢掉那批用量。
func TestAllReportsDeliveredBeforeServeReturns(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadGateway)
	}}
	url := up.start(t)
	f := newFixture(t,
		relaymock.Step{Target: target(url, "m1")},
		relaymock.Step{Target: target(url, "m2")},
		relaymock.Step{Target: target(url, "m3")},
	)

	f.p.Serve(context.Background(), httptest.NewRecorder(), call(t, true))

	// 立刻读，不等不睡：MaxAttempts 次尝试就该有 MaxAttempts 条上报。
	if got := len(f.col.outcomes()); got != 3 {
		t.Errorf("Serve 返回时上报 = %d 条，want 3（有上报还在在途）", got)
	}
}

// 轨迹必须同步追加：它写 rec，而落库读同一个 rec。
// 断言 Serve 返回时轨迹已完整。
func TestAttemptsTrailCompleteWhenServeReturns(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadGateway)
	}}
	url := up.start(t)
	f := newFixture(t,
		relaymock.Step{Target: target(url, "m1")},
		relaymock.Step{Target: target(url, "m2")},
		relaymock.Step{Target: target(url, "m3")},
	)

	f.p.Serve(context.Background(), httptest.NewRecorder(), call(t, true))

	if got := len(f.col.record(t).AttemptsTrail); got != 3 {
		t.Errorf("轨迹 = %d 项，want 3", got)
	}
}

// 记录键与回显 id 分离后，上报与 dispatch 都要用记录键：
// 四处只要有一处用会撞号的回显 id，事后就串不起同一次请求。
func TestReportAndDispatchUseRecordKey(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "m1")})

	c := call(t, true)
	c.RecordKey = "generated-key"
	f.p.Serve(context.Background(), httptest.NewRecorder(), c)

	if got := f.col.record(t).RequestID; got != "generated-key" {
		t.Errorf("流水键 = %q, want generated-key", got)
	}
	f.col.mu.Lock()
	reports := append([]relayclient.ResultReport(nil), f.col.reports...)
	f.col.mu.Unlock()
	if len(reports) != 1 {
		t.Fatalf("上报 = %d 条", len(reports))
	}
	if reports[0].RequestID != "generated-key" {
		t.Errorf("上报 request_id = %q, want generated-key", reports[0].RequestID)
	}
	if reports[0].ReportID != "generated-key:0" {
		t.Errorf("report_id = %q, want generated-key:0", reports[0].ReportID)
	}
	for _, d := range f.relay.Dispatches() {
		if d.RequestID != "generated-key" {
			t.Errorf("dispatch request_id = %q, want generated-key", d.RequestID)
		}
	}
}

// RecordKey 为空时回落到 RequestID：绝大多数请求是这个形态，不得变。
func TestEmptyRecordKeyFallsBackToRequestID(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "m1")})

	c := call(t, true)
	c.RecordKey = ""
	f.p.Serve(context.Background(), httptest.NewRecorder(), c)

	if got := f.col.record(t).RequestID; got != c.RequestID {
		t.Errorf("流水键 = %q, want %q", got, c.RequestID)
	}
}

// 端到端：连接失败时 URL 与其中的 key 不得出现在客户端体、流水与轨迹里。
//
// 在这一层断言而不是只测净化函数：真实泄漏是「构造 ir.Error 那处漏调净化」，
// 而单测净化函数对那种改动完全不敏感。
func TestConnectionFailureLeaksNoCredential(t *testing.T) {
	// key 放在 query 里——正是 AttemptRecord 拒绝记 BaseURL 的那个理由。
	dead := deadBaseURL(t) + "/v1/messages?api_key=sk-leak-me"
	f := newFixture(t,
		relaymock.Step{Target: target(dead, "m1")},
		relaymock.Step{Target: target(dead, "m2")},
		relaymock.Step{Target: target(dead, "m3")},
	)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	rec := f.col.record(t)
	haystacks := map[string]string{
		"客户端响应体":           w.Body.String(),
		"流水 error_message": rec.ErrorMessage,
	}
	for i, a := range rec.AttemptsTrail {
		haystacks[fmt.Sprintf("轨迹第 %d 项", i+1)] = a.ErrorMessage
	}
	for where, hay := range haystacks {
		if strings.Contains(hay, "sk-leak-me") {
			t.Errorf("%s 里泄漏了凭据：%s", where, hay)
		}
		if strings.Contains(hay, "://") {
			t.Errorf("%s 里泄漏了 URL：%s", where, hay)
		}
	}
	// 归因仍在：不能为了净化把「为什么失败」也抹掉。
	if rec.ErrorMessage == "" {
		t.Error("流水没记错误原因")
	}
}

// 非连接层的出站失败同样不得泄漏。open 有两条出错路径，判 isTransportError
// 分流：上面那个用死端口走的是连接层那条，这里要的是另一条。
//
// 用自签证书的 TLS 上游：证书校验失败刻意不算连接层故障
// （见 isTransportError 的注释），于是它必然落到 "upstream unreachable"。
func TestNonTransportFailureLeaksNoCredential(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { writeStream(w, okStream) }))
	t.Cleanup(srv.Close)
	base := srv.URL + "/v1/messages?api_key=sk-tls-leak"

	f := newFixture(t,
		relaymock.Step{Target: target(base, "m1")},
		relaymock.Step{Target: target(base, "m2")},
		relaymock.Step{Target: target(base, "m3")},
	)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	rec := f.col.record(t)
	if !strings.Contains(rec.ErrorMessage, "unreachable") {
		t.Fatalf("没走到非连接层那条路径：error_message = %q", rec.ErrorMessage)
	}
	haystacks := map[string]string{
		"客户端响应体":           w.Body.String(),
		"流水 error_message": rec.ErrorMessage,
	}
	for i, a := range rec.AttemptsTrail {
		haystacks[fmt.Sprintf("轨迹第 %d 项", i+1)] = a.ErrorMessage
	}
	for where, hay := range haystacks {
		if strings.Contains(hay, "sk-tls-leak") {
			t.Errorf("%s 里泄漏了凭据：%s", where, hay)
		}
		if strings.Contains(hay, "://") {
			t.Errorf("%s 里泄漏了 URL：%s", where, hay)
		}
	}
}
