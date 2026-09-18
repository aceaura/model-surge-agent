package pipeline_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/relayclient"
	"github.com/aceaura/model-surge-agent/backend/relaymock"
)

// reports 取全部上报的副本。collector 只暴露 outcomes()，
// 本轮要看的是上报上的另一个维度。
func (f *fixture) reports() []relayclient.ResultReport {
	f.col.mu.Lock()
	defer f.col.mu.Unlock()
	out := make([]relayclient.ResultReport, len(f.col.reports))
	copy(out, f.col.reports)
	return out
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func mustUnmarshal(t *testing.T, raw []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("unmarshal: %v — %s", err, raw)
	}
}

func contains(raw []byte, want string) bool {
	return strings.Contains(string(raw), want)
}

// rateLimitedUpstream 是恒回 429 的假上游，响应头可配。
func rateLimitedUpstream(t *testing.T, headers map[string]string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error",` +
			`"message":"rate limit exceeded"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestRateLimitReportCarriesRetryAfter 限流上报必须带上到期时刻。
//
// 这是本轮的核心链路：上游明示的到期时刻要一路到调度层，
// 否则调度层只能按默认时长猜，在整个限流窗口里反复空转。
func TestRateLimitReportCarriesRetryAfter(t *testing.T) {
	url := rateLimitedUpstream(t, map[string]string{"Retry-After": "300"})
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})

	before := time.Now()
	f.p.Serve(context.Background(), httptest.NewRecorder(), call(t, true))
	after := time.Now()

	reports := f.reports()
	if len(reports) == 0 {
		t.Fatal("没有任何上报")
	}
	for i, rep := range reports {
		if rep.RetryAfter.IsZero() {
			t.Fatalf("第 %d 条上报没带到期时刻：调度层只能回落到猜", i)
		}
		lo, hi := before.Add(300*time.Second), after.Add(300*time.Second)
		if rep.RetryAfter.Before(lo) || rep.RetryAfter.After(hi) {
			t.Fatalf("第 %d 条 RetryAfter = %v，应落在 [%v, %v]",
				i, rep.RetryAfter, lo, hi)
		}
	}
}

// TestEveryAttemptReportsRetryAfter 多次尝试的每一条上报都要带。
//
// 只在最后一条带是不够的：调度层按 model_id 更新运行态，
// 换目标之后前一个目标的冷却信息就再没机会送达了。
func TestEveryAttemptReportsRetryAfter(t *testing.T) {
	url := rateLimitedUpstream(t, map[string]string{"Retry-After": "120"})
	f := newFixture(t,
		relaymock.Step{Target: target(url, "kimi-1/k3")},
		relaymock.Step{Target: target(url, "kimi-2/k3")},
	)

	f.p.Serve(context.Background(), httptest.NewRecorder(), call(t, true))

	reports := f.reports()
	if len(reports) < 2 {
		t.Fatalf("上报 %d 条，本用例要求至少两次尝试", len(reports))
	}
	for i, rep := range reports {
		if rep.RetryAfter.IsZero() {
			t.Errorf("第 %d 条（目标 %s）没带到期时刻", i, rep.ModelID)
		}
	}
}

// TestNonRateLimitReportHasNoRetryAfter 没有限流头时不能编造。
//
// 上游只是坏了而不是限流时，带上一个时刻会让调度层按不存在的窗口冷却。
func TestNonRateLimitReportHasNoRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	t.Cleanup(srv.Close)

	f := newFixture(t, relaymock.Step{Target: target(srv.URL, "kimi-1/k3")})
	f.p.Serve(context.Background(), httptest.NewRecorder(), call(t, true))

	for i, rep := range f.reports() {
		if !rep.RetryAfter.IsZero() {
			t.Errorf("第 %d 条上报带了 %v，上游没说就不该有", i, rep.RetryAfter)
		}
	}
}

// TestSuccessReportHasNoRetryAfter 成功的上报恒不带到期时刻。
//
// report 的 err 参数为 nil 那一路：拿上一次失败的时刻污染成功上报
// 会让一个刚刚恢复的目标立刻又被冷却。
func TestSuccessReportHasNoRetryAfter(t *testing.T) {
	up := &fakeUpstream{handler: func(n int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		// 就算成功响应上带了限流头（真实上游在接近配额时会带），
		// 成功的上报也不该携带它——那是「还剩多少」而不是「被拒了」。
		w.Header().Set("Retry-After", "300")
		_, _ = w.Write([]byte(okStream))
	}}
	url := up.start(t)

	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})
	f.p.Serve(context.Background(), httptest.NewRecorder(), call(t, true))

	reports := f.reports()
	if len(reports) != 1 {
		t.Fatalf("上报 %d 条，want 1", len(reports))
	}
	if !reports[0].RetryAfter.IsZero() {
		t.Fatalf("成功上报带了 %v", reports[0].RetryAfter)
	}
	if reports[0].Outcome != relayclient.OutcomeNormal {
		t.Fatalf("outcome = %q, want normal", reports[0].Outcome)
	}
}

// TestRecordKeepsRetryAfter 流水要记下到期时刻。
//
// 事后要能回答「那次为什么换了目标」，没有这一列只能看到 outcome=retrying。
func TestRecordKeepsRetryAfter(t *testing.T) {
	url := rateLimitedUpstream(t, map[string]string{"Retry-After": "90"})
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})

	f.p.Serve(context.Background(), httptest.NewRecorder(), call(t, true))

	rec := f.col.record(t)
	if rec.RetryAfter.IsZero() {
		t.Fatal("流水没记到期时刻")
	}
}

// TestRetryAfterSurvivesJSONRoundTrip 上报要真的能过网线。
//
// time.Time 带 omitzero：零值不出现在 JSON 里（调度层据此回落启发式），
// 非零值必须能被对端解回来。只测结构体字段而不测序列化，
// 会漏掉 tag 写错这一类问题——而那会让整条链路静默失效。
func TestRetryAfterSurvivesJSONRoundTrip(t *testing.T) {
	t.Run("非零值往返", func(t *testing.T) {
		at := time.Now().Add(5 * time.Minute).UTC().Truncate(time.Second)
		raw := mustMarshal(t, relayclient.ResultReport{
			ReportID: "r1", ModelID: "m", Outcome: relayclient.OutcomeRetrying,
			RetryAfter: at,
		})
		if !contains(raw, "retry_after") {
			t.Fatalf("JSON 里没有 retry_after 键：%s", raw)
		}
		var back relayclient.ResultReport
		mustUnmarshal(t, raw, &back)
		if !back.RetryAfter.Equal(at) {
			t.Fatalf("往返后 = %v, want %v", back.RetryAfter, at)
		}
	})

	t.Run("零值不出现在 JSON 里", func(t *testing.T) {
		raw := mustMarshal(t, relayclient.ResultReport{
			ReportID: "r1", ModelID: "m", Outcome: relayclient.OutcomeAbnormal,
		})
		if contains(raw, "retry_after") {
			t.Fatalf("零值也写出了键，调度层会把公元 1 年当成到期时刻：%s", raw)
		}
	})
}
