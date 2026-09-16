package relayclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/relayclient"
	"github.com/aceaura/model-surge-agent/backend/relaymock"
)

const dispatchKey = "dispatch-key-1"

func target(modelID string) relayclient.Target {
	return relayclient.Target{
		ModelID:       modelID,
		Account:       "kimi-1",
		ProviderID:    "moonshot",
		Protocol:      "anthropic",
		BaseURL:       "https://api.example.test/coding",
		NativeModel:   "kimi-k3-256k",
		ContextWindow: 262144,
		Headers:       map[string]string{"x-api-key": "sk-secret-value-1234"},
		Defaults:      json.RawMessage(`{"temperature":0.6}`),
		Overrides:     json.RawMessage(`{"max_tokens":8192}`),
	}
}

func newClient(t *testing.T, m *relaymock.Mock) *relayclient.Client {
	t.Helper()
	m.DispatchKey = dispatchKey
	srv := m.Start()
	t.Cleanup(srv.Close)
	return relayclient.New(srv.URL, dispatchKey)
}

func TestDispatchCarriesEveryTargetField(t *testing.T) {
	m := relaymock.New(relaymock.Step{Target: target("kimi-1/k3")})
	c := newClient(t, m)

	resp, err := c.Dispatch(context.Background(), relayclient.DispatchRequest{
		Model: "kimi-k3", InboundProtocol: "anthropic",
		ClientKey: "ck-1", RequestID: "req-1", EstTokens: 1200,
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !reflect.DeepEqual(resp.Target, target("kimi-1/k3")) {
		t.Fatalf("target = %+v", resp.Target)
	}
	if resp.RequestID != "req-1" {
		t.Errorf("request_id = %q", resp.RequestID)
	}

	got := m.Dispatches()
	if len(got) != 1 {
		t.Fatalf("dispatch count = %d", len(got))
	}
	if got[0].ClientKey != "ck-1" || got[0].EstTokens != 1200 || got[0].InboundProtocol != "anthropic" {
		t.Errorf("request lost fields: %+v", got[0])
	}
}

// 换目标靠 tried_ids 累积，没有租约。这条断言保护那个契约。
func TestTriedIDsAccumulateAcrossAttempts(t *testing.T) {
	m := relaymock.New(
		relaymock.Step{Target: target("kimi-1/k3")},
		relaymock.Step{Target: target("ark-1/ds")},
		relaymock.Step{Err: &relayclient.Error{
			Code: relayclient.CodeTargetUnavailable, Retryable: true, Message: "no candidates left"}},
	)
	c := newClient(t, m)

	var tried []string
	for attempt := range 3 {
		resp, err := c.Dispatch(context.Background(), relayclient.DispatchRequest{
			Model: "kimi-k3", RequestID: "req-1", TriedIDs: tried,
		})
		if err != nil {
			var re *relayclient.Error
			if !errors.As(err, &re) {
				t.Fatalf("attempt %d: unexpected error type %T", attempt, err)
			}
			if !re.Exhausted() {
				t.Fatalf("attempt %d: %v", attempt, err)
			}
			break
		}
		tried = append(tried, resp.Target.ModelID)
	}

	dispatches := m.Dispatches()
	want := [][]string{nil, {"kimi-1/k3"}, {"kimi-1/k3", "ark-1/ds"}}
	if len(dispatches) != len(want) {
		t.Fatalf("dispatch count = %d, want %d", len(dispatches), len(want))
	}
	for i, w := range want {
		if !reflect.DeepEqual(dispatches[i].TriedIDs, w) {
			t.Errorf("dispatch[%d].tried_ids = %v, want %v", i, dispatches[i].TriedIDs, w)
		}
	}
	// request_id 全程不变：多次 dispatch 属于同一个客户端请求。
	for i, d := range dispatches {
		if d.RequestID != "req-1" {
			t.Errorf("dispatch[%d].request_id = %q, must stay stable", i, d.RequestID)
		}
	}
}

func TestReportIsRecordedVerbatim(t *testing.T) {
	m := relaymock.New()
	c := newClient(t, m)

	rep := relayclient.ResultReport{
		ReportID: "req-1:0", RequestID: "req-1", ModelID: "kimi-1/k3",
		Outcome: relayclient.OutcomeNormal,
		Usage:   relayclient.Usage{InputTokens: 120, OutputTokens: 64, CacheReadTokens: 80},
	}
	if err := c.Report(context.Background(), rep); err != nil {
		t.Fatalf("Report: %v", err)
	}
	got := m.Reports()
	if len(got) != 1 || !reflect.DeepEqual(got[0], rep) {
		t.Fatalf("reports = %+v", got)
	}
}

// 上报失败要能被识别成可重试，数据面据此决定入 outbox 而非丢弃。
func TestReportFailureIsRetryable(t *testing.T) {
	m := relaymock.New()
	m.FailReports(&relayclient.Error{
		Code: relayclient.CodeInternal, Retryable: true, Message: "db down"})
	c := newClient(t, m)

	err := c.Report(context.Background(), relayclient.ResultReport{ReportID: "req-1:0"})
	var re *relayclient.Error
	if !errors.As(err, &re) {
		t.Fatalf("error = %v (%T)", err, err)
	}
	if !re.Retryable {
		t.Errorf("report failure must be retryable: %+v", re)
	}
	if len(m.Reports()) != 0 {
		t.Errorf("failed report must not be recorded")
	}
}

func TestModels(t *testing.T) {
	m := relaymock.New()
	m.Models = []relayclient.UserModelSummary{
		{Name: "kimi-k3", Collection: "c1", Policy: "failover", Protocol: "anthropic", Enabled: true},
	}
	c := newClient(t, m)

	got, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(got) != 1 || got[0].Name != "kimi-k3" || !got[0].Enabled {
		t.Fatalf("models = %+v", got)
	}
	if !c.Ready(context.Background()) {
		t.Error("Ready must be true while the mock is up")
	}
}

func TestBadDispatchKeyIsNotRetryable(t *testing.T) {
	m := relaymock.New(relaymock.Step{Target: target("kimi-1/k3")})
	m.DispatchKey = dispatchKey
	srv := m.Start()
	defer srv.Close()

	c := relayclient.New(srv.URL, "wrong-key")
	_, err := c.Dispatch(context.Background(), relayclient.DispatchRequest{Model: "m"})
	var re *relayclient.Error
	if !errors.As(err, &re) {
		t.Fatalf("error = %v (%T)", err, err)
	}
	if re.Code != relayclient.CodeUnauthorized {
		t.Errorf("code = %q", re.Code)
	}
	if re.Retryable {
		t.Error("a bad key will never succeed on retry")
	}
}

// relay 挂掉与候选耗尽必须区分：前者稍后可自愈，后者重试循环要立刻停。
func TestUnreachableRelayIsRetryableButNotExhausted(t *testing.T) {
	srv := httptest.NewServer(nil)
	url := srv.URL
	srv.Close()

	c := relayclient.New(url, dispatchKey)
	_, err := c.Dispatch(context.Background(), relayclient.DispatchRequest{Model: "m"})
	var re *relayclient.Error
	if !errors.As(err, &re) {
		t.Fatalf("error = %v (%T)", err, err)
	}
	if !re.Retryable {
		t.Error("an unreachable relay is worth retrying")
	}
	if re.Exhausted() {
		t.Error("unreachable is not the same as out of candidates")
	}
}

// 反代返回非 JSON 时也要归出错误码，不能因解不出信封而 panic 或静默成功。
func TestNonJSONErrorFallsBackToStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>gateway down</html>"))
	}))
	defer srv.Close()

	c := relayclient.New(srv.URL, dispatchKey)
	_, err := c.Dispatch(context.Background(), relayclient.DispatchRequest{Model: "m"})
	var re *relayclient.Error
	if !errors.As(err, &re) {
		t.Fatalf("error = %v (%T)", err, err)
	}
	if re.Code != relayclient.CodeInternal || !re.Retryable {
		t.Errorf("error = %+v", re)
	}
	if !strings.Contains(re.Message, "502") {
		t.Errorf("message should mention the status: %q", re.Message)
	}
}

// 凭据只能进出站请求头，日志里必须是脱敏形态。
func TestTargetStringRedactsCredentials(t *testing.T) {
	s := target("kimi-1/k3").String()
	if strings.Contains(s, "sk-secret-value-1234") {
		t.Fatalf("String() leaks the credential: %s", s)
	}
	if !strings.Contains(s, "x-api-key") {
		t.Errorf("header names should survive for diagnosis: %s", s)
	}
}

func TestRedactHeadersAcrossCasings(t *testing.T) {
	for _, name := range []string{"Authorization", "authorization", "X-Api-Key", "x-goog-api-key"} {
		t.Run(name, func(t *testing.T) {
			got := relayclient.RedactHeaders(map[string]string{name: "secret-token-abcd"})
			if got[name] == "secret-token-abcd" {
				t.Fatalf("%s not redacted: %v", name, got)
			}
		})
	}
}

// 序列化必须保留原值：这份 target 要被拿去发真请求。
func TestTargetMarshalKeepsCredentials(t *testing.T) {
	raw, err := json.Marshal(target("kimi-1/k3"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "sk-secret-value-1234") {
		t.Fatalf("marshalled target must carry the real credential: %s", raw)
	}
	for _, key := range []string{"model_id", "base_url", "native_model", "headers", "defaults", "overrides"} {
		if !strings.Contains(string(raw), `"`+key+`"`) {
			t.Errorf("field %q missing from %s", key, raw)
		}
	}
}
