package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/codec"
	_ "github.com/aceaura/model-surge-agent/backend/codec/anthropic"
	"github.com/aceaura/model-surge-agent/backend/pipeline"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
	"github.com/aceaura/model-surge-agent/backend/relaymock"
)

// ---- 测试替身 ----

// fakeUpstream 是可编程的假上游 LLM，记录收到的请求体。
type fakeUpstream struct {
	mu     sync.Mutex
	bodies []map[string]any
	// handler 按调用序号决定怎么回应。
	handler func(n int, w http.ResponseWriter)
	served  int
}

func (f *fakeUpstream) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)

		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		n := f.served
		f.served++
		f.mu.Unlock()

		f.handler(n, w)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (f *fakeUpstream) body(t *testing.T, n int) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if n >= len(f.bodies) {
		t.Fatalf("upstream got %d requests, wanted at least %d", len(f.bodies), n+1)
	}
	return f.bodies[n]
}

func (f *fakeUpstream) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

// collector 收集上报与流水，替代真正的 outbox 与 PG。
type collector struct {
	mu      sync.Mutex
	reports []relayclient.ResultReport
	records []pipeline.Record
}

func (c *collector) Report(rep relayclient.ResultReport) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reports = append(c.reports, rep)
}

func (c *collector) Record(rec pipeline.Record) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, rec)
}

func (c *collector) outcomes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.reports))
	for i, r := range c.reports {
		out[i] = r.Outcome
	}
	return out
}

func (c *collector) record(t *testing.T) pipeline.Record {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.records) != 1 {
		t.Fatalf("want exactly one record, got %d", len(c.records))
	}
	return c.records[0]
}

// ---- 固定装置 ----

const okStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"kimi-k3-256k","usage":{"input_tokens":100}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}

`

func writeStream(w http.ResponseWriter, raw string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(raw))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func target(baseURL, modelID string) relayclient.Target {
	return relayclient.Target{
		ModelID:     modelID,
		Account:     "acc-1",
		Protocol:    codec.ProtocolAnthropic,
		BaseURL:     baseURL,
		NativeModel: "kimi-k3-256k",
		Headers:     map[string]string{"x-api-key": "sk-secret-1234"},
	}
}

type fixture struct {
	p     *pipeline.Pipeline
	relay *relaymock.Mock
	col   *collector
}

func newFixture(t *testing.T, steps ...relaymock.Step) *fixture {
	t.Helper()
	m := relaymock.New(steps...)
	srv := m.Start()
	t.Cleanup(srv.Close)
	col := &collector{}
	return &fixture{
		p: &pipeline.Pipeline{
			Dispatch: relayclient.New(srv.URL, ""),
			Reporter: col,
			Recorder: col,
			Opts: pipeline.Options{
				MaxAttempts:       3,
				FirstTokenTimeout: 2 * time.Second,
				IdleTimeout:       2 * time.Second,
			},
		},
		relay: m,
		col:   col,
	}
}

func call(t *testing.T, stream bool) pipeline.Call {
	t.Helper()
	in, ok := codec.Inbound(codec.ProtocolAnthropic)
	if !ok {
		t.Fatal("anthropic inbound codec not registered")
	}
	body := fmt.Sprintf(`{"model":"kimi-k3","max_tokens":1024,"stream":%v,
      "messages":[{"role":"user","content":"hi"}]}`, stream)
	req, err := in.DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	return pipeline.Call{
		RequestID: "req-1",
		Protocol:  codec.ProtocolAnthropic,
		Inbound:   in,
		Request:   req,
		UserModel: "kimi-k3",
		ClientKey: "ck-1",
		Stream:    stream,
	}
}

// ---- 正常路径 ----

func TestStreamingHappyPath(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q", ct)
	}
	out := w.Body.String()
	for _, want := range []string{"message_start", "content_block_start", "hello", "message_stop"} {
		if !strings.Contains(out, want) {
			t.Errorf("response missing %q:\n%s", want, out)
		}
	}

	if got := f.col.outcomes(); len(got) != 1 || got[0] != relayclient.OutcomeNormal {
		t.Errorf("outcomes = %v", got)
	}
	rec := f.col.record(t)
	if !rec.Committed || rec.Attempts != 1 || rec.ModelID != "kimi-1/k3" {
		t.Errorf("record = %+v", rec)
	}
	if rec.Usage.InputTokens != 100 || rec.Usage.OutputTokens != 7 {
		t.Errorf("usage = %+v", rec.Usage)
	}
	if rec.Outcome != relayclient.OutcomeNormal || rec.OutboundProtocol != codec.ProtocolAnthropic {
		t.Errorf("record = %+v", rec)
	}
}

// 客户端要非流式时由数据面聚合，对上游依然是流式请求。
func TestNonStreamingAggregates(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, false))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
	var resp struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v — %s", err, w.Body)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "hello" {
		t.Errorf("content = %+v", resp.Content)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q", resp.StopReason)
	}

	// 对上游永远流式，与客户端的选择无关。
	if got := up.body(t, 0)["stream"]; got != true {
		t.Errorf("upstream request stream = %v, must always be true", got)
	}
}

// 关键不变式：native model 在编码前替换，参数覆盖在编码后作用于 wire body。
func TestNativeModelAndParamLayersReachTheWireBody(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	url := up.start(t)

	tgt := target(url, "kimi-1/k3")
	tgt.Defaults = json.RawMessage(`{"temperature":0.6,"max_tokens":9999}`)
	tgt.Overrides = json.RawMessage(`{"thinking":{"type":"enabled","budget_tokens":512}}`)
	f := newFixture(t, relaymock.Step{Target: tgt})

	f.p.Serve(context.Background(), httptest.NewRecorder(), call(t, true))

	body := up.body(t, 0)
	if body["model"] != "kimi-k3-256k" {
		t.Errorf("model = %v, want the native model", body["model"])
	}
	// default 只填缺失键：客户端给了 max_tokens:1024，必须保留。
	if body["max_tokens"] != float64(1024) {
		t.Errorf("max_tokens = %v, a default must not overwrite a client value", body["max_tokens"])
	}
	if body["temperature"] != 0.6 {
		t.Errorf("temperature = %v, a default must fill a missing key", body["temperature"])
	}
	// override 无条件写入，且能表达出站协议特有的嵌套结构。
	th, _ := body["thinking"].(map[string]any)
	if th == nil || th["budget_tokens"] != float64(512) {
		t.Errorf("thinking = %v, an override must reach the nested wire field", body["thinking"])
	}
}

// 凭据只进出站请求头，不进流水记录。
func TestCredentialsNeverReachTheRecord(t *testing.T) {
	var gotKey string
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})
	f.p.HTTP = &http.Client{Transport: capture(&gotKey)}

	f.p.Serve(context.Background(), httptest.NewRecorder(), call(t, true))

	if gotKey != "sk-secret-1234" {
		t.Errorf("credential must reach the upstream request, got %q", gotKey)
	}
	raw, err := json.Marshal(f.col.record(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sk-secret-1234") {
		t.Fatalf("credential leaked into the record: %s", raw)
	}
}

// 畸形请求在编码前就被修好，修复说明落进流水。
func TestSanitizeDiagnosticsLandInTheRecord(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})

	in, ok := codec.Inbound(codec.ProtocolAnthropic)
	if !ok {
		t.Fatal("anthropic inbound codec not registered")
	}
	// tool_result 指向一个从未出现过的 tool_use：上游会拒收整个请求。
	body := `{"model":"kimi-k3","max_tokens":1024,"stream":true,"messages":[
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_gone",
	     "content":[{"type":"text","text":"3 hits"}]}]}]}`
	req, err := in.DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	f.p.Serve(context.Background(), httptest.NewRecorder(), pipeline.Call{
		RequestID: "req-1",
		Protocol:  codec.ProtocolAnthropic,
		Inbound:   in,
		Request:   req,
		UserModel: "kimi-k3",
		ClientKey: "ck-1",
		Stream:    true,
	})

	rec := f.col.record(t)
	if len(rec.Sanitized) == 0 {
		t.Fatal("want a sanitize diagnostic for the orphan tool_result")
	}
	if !strings.Contains(strings.Join(rec.Sanitized, "; "), "orphan") {
		t.Fatalf("diagnostics must name the orphan: %v", rec.Sanitized)
	}
	raw, err := json.Marshal(up.body(t, 0))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "tool_result") {
		t.Fatalf("the orphan must be gone before encoding: %s", raw)
	}
}

// 目标协议承不住的字段要记进 Lossy，且不能混进 Sanitized：
// 请求本身合法，问题在路由选到了表达不了它的目标。
func TestLossyRecordsWhatTheTargetCannotExpress(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})

	in, ok := codec.Inbound(codec.ProtocolAnthropic)
	if !ok {
		t.Fatal("anthropic inbound codec not registered")
	}
	// document 容器里装音频：合法的客户端请求，但 Anthropic 不收这个类型。
	body := `{"model":"kimi-k3","max_tokens":1024,"stream":true,"messages":[
	  {"role":"user","content":[
	    {"type":"text","text":"transcribe this"},
	    {"type":"document","source":{"type":"base64","media_type":"audio/wav","data":"UklGRg=="}}]}]}`
	req, err := in.DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	f.p.Serve(context.Background(), httptest.NewRecorder(), pipeline.Call{
		RequestID: "req-1",
		Protocol:  codec.ProtocolAnthropic,
		Inbound:   in,
		Request:   req,
		UserModel: "kimi-k3",
		ClientKey: "ck-1",
		Stream:    true,
	})

	rec := f.col.record(t)
	if len(rec.Lossy) == 0 {
		t.Fatal("want a lossy diagnostic for the unsupported media type")
	}
	if !strings.Contains(strings.Join(rec.Lossy, "; "), "audio") {
		t.Errorf("diagnostics must name the dropped block type: %v", rec.Lossy)
	}
	if len(rec.Sanitized) != 0 {
		t.Errorf("a legal request must not be reported as sanitized: %v", rec.Sanitized)
	}
}

// 无损转换不产出 Lossy 说明。
func TestLossyIsEmptyForLosslessConversion(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})

	f.p.Serve(context.Background(), httptest.NewRecorder(), call(t, true))

	if got := f.col.record(t).Lossy; len(got) != 0 {
		t.Fatalf("lossless conversion must produce no diagnostics, got %v", got)
	}
}

// 合法请求不被改动，也不产出诊断。
func TestSanitizeLeavesHealthyRequestsUnreported(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})

	f.p.Serve(context.Background(), httptest.NewRecorder(), call(t, true))

	if got := f.col.record(t).Sanitized; len(got) != 0 {
		t.Fatalf("healthy request must produce no diagnostics, got %v", got)
	}
}

type captureTransport struct {
	key *string
}

func (c captureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	*c.key = r.Header.Get("x-api-key")
	return http.DefaultTransport.RoundTrip(r)
}

func capture(key *string) http.RoundTripper { return captureTransport{key: key} }

// ---- 换目标 ----

// pre-write 阶段的失败必须换目标，且客户端不能看到 200。
func TestPreWriteFailureSwapsTarget(t *testing.T) {
	up := &fakeUpstream{handler: func(n int, w http.ResponseWriter) {
		if n == 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"overloaded"}}`))
			return
		}
		writeStream(w, okStream)
	}}
	url := up.start(t)
	f := newFixture(t,
		relaymock.Step{Target: target(url, "kimi-1/k3")},
		relaymock.Step{Target: target(url, "ark-1/ds")},
	)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "hello") {
		t.Errorf("the retry's content should reach the client: %s", w.Body)
	}

	// 第二次 dispatch 必须带上失败目标。
	dispatches := f.relay.Dispatches()
	if len(dispatches) != 2 {
		t.Fatalf("dispatch count = %d", len(dispatches))
	}
	if len(dispatches[1].TriedIDs) != 1 || dispatches[1].TriedIDs[0] != "kimi-1/k3" {
		t.Errorf("tried_ids = %v", dispatches[1].TriedIDs)
	}
	// request_id 全程不变，report_id 带 attempt 区分。
	reports := f.col.reports
	if len(reports) != 2 {
		t.Fatalf("reports = %+v", reports)
	}
	if reports[0].Outcome != relayclient.OutcomeRetrying || reports[0].ReportID != "req-1:0" {
		t.Errorf("first report = %+v", reports[0])
	}
	if reports[1].Outcome != relayclient.OutcomeNormal || reports[1].ReportID != "req-1:1" {
		t.Errorf("second report = %+v", reports[1])
	}
}

// 首帧解码失败也算 pre-write：还没写 200，仍能换目标。
func TestUndecodableFirstFrameSwapsTarget(t *testing.T) {
	up := &fakeUpstream{handler: func(n int, w http.ResponseWriter) {
		if n == 0 {
			writeStream(w, "event: message_start\ndata: {not json\n\n")
			return
		}
		writeStream(w, okStream)
	}}
	url := up.start(t)
	f := newFixture(t,
		relaymock.Step{Target: target(url, "kimi-1/k3")},
		relaymock.Step{Target: target(url, "ark-1/ds")},
	)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "hello") {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	if up.calls() != 2 {
		t.Errorf("upstream calls = %d, want a second attempt", up.calls())
	}
	if got := f.col.record(t).ModelID; got != "ark-1/ds" {
		t.Errorf("record should name the target that succeeded, got %q", got)
	}
}

// 无对应出站 codec：这个目标永远不可用，但换一个可能行。
func TestUnknownProtocolReportsInvalidModelAndSwaps(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	url := up.start(t)

	bad := target(url, "ghost/x")
	bad.Protocol = "telepathy"
	f := newFixture(t,
		relaymock.Step{Target: bad},
		relaymock.Step{Target: target(url, "kimi-1/k3")},
	)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	got := f.col.outcomes()
	if len(got) != 2 || got[0] != relayclient.OutcomeInvalidModel {
		t.Errorf("outcomes = %v", got)
	}
}

// 尝试次数用尽后最后一次上报要降成 abnormal：
// retrying 会让调度层以为还有后续尝试。
func TestExhaustedAttemptsDowngradeToAbnormal(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadGateway)
	}}
	url := up.start(t)
	f := newFixture(t,
		relaymock.Step{Target: target(url, "m1")},
		relaymock.Step{Target: target(url, "m2")},
		relaymock.Step{Target: target(url, "m3")},
	)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if w.Code == http.StatusOK {
		t.Fatalf("nothing succeeded, must not report 200: %s", w.Body)
	}
	want := []string{relayclient.OutcomeRetrying, relayclient.OutcomeRetrying, relayclient.OutcomeAbnormal}
	if got := f.col.outcomes(); !equal(got, want) {
		t.Errorf("outcomes = %v, want %v", got, want)
	}
	if up.calls() != 3 {
		t.Errorf("upstream calls = %d, want MaxAttempts", up.calls())
	}
}

// 候选耗尽时立刻停：再问一次只会得到同样的答案。
func TestCandidatesExhaustedStopsImmediately(t *testing.T) {
	f := newFixture(t, relaymock.Step{Err: &relayclient.Error{
		Code: relayclient.CodeTargetUnavailable, Retryable: true, Message: "no candidates"}})

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if w.Code == http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if got := len(f.relay.Dispatches()); got != 1 {
		t.Errorf("dispatch count = %d, must not keep asking", got)
	}
	if got := f.col.outcomes(); len(got) != 0 {
		t.Errorf("no target was handed out, nothing to report: %v", got)
	}
}

// ---- 不换目标的失败 ----

// 上下文超限不换目标、不重试，且完全不改运行态。
func TestContextExceededDoesNotRetry(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error",` +
			`"message":"prompt is too long: 300000 tokens"}}`))
	}}
	url := up.start(t)
	f := newFixture(t,
		relaymock.Step{Target: target(url, "kimi-1/k3")},
		relaymock.Step{Target: target(url, "ark-1/ds")},
	)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want the upstream's 400: %s", w.Code, w.Body)
	}
	if up.calls() != 1 {
		t.Errorf("upstream calls = %d, an oversized prompt will not fit elsewhere either", up.calls())
	}
	got := f.col.outcomes()
	if len(got) != 1 || got[0] != relayclient.OutcomeContextExceeded {
		t.Errorf("outcomes = %v", got)
	}
	if rec := f.col.record(t); rec.Committed {
		t.Error("nothing was written, must not be marked committed")
	}
}

// 上游 404 归 invalid_model。
func TestUpstreamNotFoundIsInvalidModel(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"not_found_error","message":"no such model"}}`))
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "ghost/x")})

	f.p.Serve(context.Background(), httptest.NewRecorder(), call(t, true))

	got := f.col.outcomes()
	if len(got) != 1 || got[0] != relayclient.OutcomeInvalidModel {
		t.Errorf("outcomes = %v", got)
	}
}

// ---- committed 之后 ----

// committed 之后断流：状态码已是 200 不可改，错误只能落在流内，
// 且必须补齐未闭合的块。
func TestErrorAfterCommitLandsInsideTheStream(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, `event: message_start
data: {"type":"message_start","message":{"id":"msg_1"}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}

event: error
data: {"type":"error","error":{"type":"api_error","message":"upstream exploded"}}

`)
	}}
	url := up.start(t)
	f := newFixture(t,
		relaymock.Step{Target: target(url, "kimi-1/k3")},
		relaymock.Step{Target: target(url, "ark-1/ds")},
	)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if w.Code != http.StatusOK {
		t.Fatalf("status was already committed to 200, got %d", w.Code)
	}
	out := w.Body.String()
	if !strings.Contains(out, "partial") {
		t.Errorf("content received before the error must reach the client: %s", out)
	}
	if !strings.Contains(out, "upstream exploded") {
		t.Errorf("the error must appear inside the stream: %s", out)
	}
	// 错误帧之前要闭合已开的块，客户端 SDK 的块状态机才能收束。
	// 闭合与宣告正常结束是两件事，下一条断言守的是后者不发生。
	if !strings.Contains(out, "content_block_stop") {
		t.Errorf("已开的块必须在错误帧之前收到闭合帧: %s", out)
	}
	if strings.Index(out, "content_block_stop") > strings.Index(out, "upstream exploded") {
		t.Errorf("闭合帧必须排在错误帧之前: %s", out)
	}
	// 错误事件本身就是本协议的终止形态。再补 message_stop 会让客户端
	// 把这轮当正常结束，把残缺内容存进历史。
	if strings.Contains(out, "message_stop") {
		t.Errorf("an error-terminated stream must not carry a normal terminator: %s", out)
	}
	if up.calls() != 1 {
		t.Errorf("upstream calls = %d, must not swap targets after commit", up.calls())
	}
	got := f.col.outcomes()
	if len(got) != 1 || got[0] != relayclient.OutcomeAbnormal {
		t.Errorf("outcomes = %v", got)
	}
}

// 上游没发 message_stop 就断开：数据面要补齐终止帧。
func TestTruncatedStreamIsClosedOut(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, `event: message_start
data: {"type":"message_start","message":{"id":"msg_1"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"cut off"}}

`)
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	out := w.Body.String()
	if !strings.Contains(out, "cut off") {
		t.Errorf("partial content must reach the client: %s", out)
	}
	if !strings.Contains(out, "message_stop") {
		t.Errorf("a truncated stream must still be terminated: %s", out)
	}
	// 半句文本是可接受的截断，但终止原因必须说成 max_tokens：
	// 报 end_turn 等于告诉客户端这段话说完了，它就不会去续写。
	if !strings.Contains(out, `"stop_reason":"max_tokens"`) {
		t.Errorf("a truncated text stream must report max_tokens: %s", out)
	}
}

// truncatedToolStream 是工具入参发到一半就终止的上游流。
const truncatedToolStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1"}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu_1","name":"read"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}

event: message_stop
data: {"type":"message_stop"}

`

// 非流式客户端还没收到任何字节，截断的工具入参可以换目标重来。
func TestTruncatedToolInputSwapsTargetWhenNotStreaming(t *testing.T) {
	up := &fakeUpstream{handler: func(n int, w http.ResponseWriter) {
		if n == 0 {
			writeStream(w, truncatedToolStream)
			return
		}
		writeStream(w, okStream)
	}}
	url := up.start(t)
	f := newFixture(t,
		relaymock.Step{Target: target(url, "kimi-1/k3")},
		relaymock.Step{Target: target(url, "ark-1/ds")},
	)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, false))

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "hello") {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	if got := f.col.outcomes(); len(got) != 2 || got[0] != relayclient.OutcomeRetrying {
		t.Errorf("outcomes = %v, the truncated attempt must be retried", got)
	}
	if up.calls() != 2 {
		t.Errorf("upstream calls = %d, want 2", up.calls())
	}
}

// 流式客户端已经收到内容、状态码已定：只能用流内错误收尾。
// 补一个 message_stop 会把残缺的工具调用伪装成完整回答，客户端会存进历史。
func TestTruncatedToolInputEndsStreamWithError(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, truncatedToolStream)
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	body := w.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Errorf("the stream must terminate with an error event: %s", body)
	}
	if strings.Contains(body, "message_stop") {
		t.Errorf("a normal terminator would present the truncation as success: %s", body)
	}
	if got := f.col.outcomes(); len(got) != 1 || got[0] != relayclient.OutcomeAbnormal {
		t.Errorf("outcomes = %v, want one abnormal", got)
	}
	if code := f.col.record(t).ErrorCode; code != "incomplete_stream" {
		t.Errorf("error_code = %q, the truncation must be recorded", code)
	}
}

// 客户端自己按了停止：上游一路正常，不该记成上游失败去累计冷却，
// 且已经产生的用量要照实记账。
func TestClientDisconnectIsAccountedAsNormal(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})

	w := &brokenWriter{ResponseRecorder: httptest.NewRecorder()}
	f.p.Serve(context.Background(), w, call(t, true))

	if got := f.col.outcomes(); len(got) != 1 || got[0] != relayclient.OutcomeNormal {
		t.Fatalf("outcomes = %v, a client hang-up is not an upstream failure", got)
	}
	if got := f.col.record(t).Usage.InputTokens; got == 0 {
		t.Errorf("input tokens = %d, the usage already received must be billed", got)
	}
}

// brokenWriter 在第一次写入后开始报错，模拟客户端中途断开。
type brokenWriter struct {
	*httptest.ResponseRecorder
	writes int
}

func (w *brokenWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > 1 {
		return 0, errors.New("connection reset by peer")
	}
	return w.ResponseRecorder.Write(p)
}

// HTTP 200 却一个事件都没解出来：上游在写 body 之前就断了。
// 这是截断而非空回答，一个字节都还没写给客户端，可以换目标。
func TestEmptyBodyOnHTTP200SwapsTarget(t *testing.T) {
	up := &fakeUpstream{handler: func(n int, w http.ResponseWriter) {
		if n == 0 {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			return
		}
		writeStream(w, okStream)
	}}
	url := up.start(t)
	f := newFixture(t,
		relaymock.Step{Target: target(url, "kimi-1/k3")},
		relaymock.Step{Target: target(url, "ark-1/ds")},
	)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "hello") {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	if got := f.col.outcomes(); len(got) != 2 || got[0] != relayclient.OutcomeRetrying {
		t.Errorf("outcomes = %v, an empty 200 must be retried", got)
	}
}

// 首字节超时属于 pre-write：还没写 200，可以换目标。
func TestFirstTokenTimeoutSwapsTarget(t *testing.T) {
	up := &fakeUpstream{handler: func(n int, w http.ResponseWriter) {
		if n == 0 {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(500 * time.Millisecond)
			return
		}
		writeStream(w, okStream)
	}}
	url := up.start(t)
	f := newFixture(t,
		relaymock.Step{Target: target(url, "kimi-1/k3")},
		relaymock.Step{Target: target(url, "ark-1/ds")},
	)
	f.p.Opts.FirstTokenTimeout = 100 * time.Millisecond

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "hello") {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	if got := f.col.outcomes(); len(got) != 2 || got[0] != relayclient.OutcomeRetrying {
		t.Errorf("outcomes = %v", got)
	}
}

// 空闲超时与首字节超时对称，但处置相反：已 committed，状态码是 200，
// 换目标会让客户端看到两段拼接的回答，所以只能流内报错收尾。
func TestIdleTimeoutAfterCommitStaysInsideTheStream(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, `event: message_start
data: {"type":"message_start","message":{"id":"msg_1"}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}

`)
		// 发完就挂住，不给终止帧，逼出空闲超时。
		time.Sleep(500 * time.Millisecond)
	}}
	url := up.start(t)
	f := newFixture(t,
		relaymock.Step{Target: target(url, "kimi-1/k3")},
		relaymock.Step{Target: target(url, "ark-1/ds")},
	)
	f.p.Opts.IdleTimeout = 100 * time.Millisecond

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if w.Code != http.StatusOK {
		t.Fatalf("committed 之后状态码不可改，got %d", w.Code)
	}
	out := w.Body.String()
	if !strings.Contains(out, "partial") {
		t.Errorf("超时前已收到的内容必须交给客户端: %s", out)
	}
	if up.calls() != 1 {
		t.Errorf("upstream calls = %d, committed 之后不该换目标", up.calls())
	}
	if rec := f.col.record(t); rec.ErrorCode != "timeout" {
		t.Errorf("error_code = %q, want timeout", rec.ErrorCode)
	}
	if strings.Contains(out, "message_stop") {
		t.Errorf("超时收尾不得宣告正常结束: %s", out)
	}
}

// 上游没给 usage 时估算兜底，并在流水里标记出来。
func TestUsageIsEstimatedWhenUpstreamOmitsIt(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, `event: message_start
data: {"type":"message_start","message":{"id":"msg_1"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a fairly long answer here"}}

event: message_stop
data: {"type":"message_stop"}

`)
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})
	f.p.Opts.EstimateUsage = true

	f.p.Serve(context.Background(), httptest.NewRecorder(), call(t, true))

	rec := f.col.record(t)
	if !rec.UsageEstimated {
		t.Error("estimated usage must be flagged so stats can be read honestly")
	}
	if rec.Usage.OutputTokens == 0 {
		t.Error("estimate should be non-zero for a non-empty answer")
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
