package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/codec"
	_ "github.com/aceaura/model-surge-agent/backend/codec/anthropic"
	_ "github.com/aceaura/model-surge-agent/backend/codec/chatcompletions"
	_ "github.com/aceaura/model-surge-agent/backend/codec/gemini"
	_ "github.com/aceaura/model-surge-agent/backend/codec/responses"
	"github.com/aceaura/model-surge-agent/backend/httpapi"
	"github.com/aceaura/model-surge-agent/backend/pipeline"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
	"github.com/aceaura/model-surge-agent/backend/relaymock"
)

// 这里测的是「路径认对了协议、凭据取对了地方、清单外形对得上」。
// 转换正确性属于 codec 的矩阵，重试与 committed 属于 pipeline，都不在这里重复。

// upstreamStream 是假上游的回答，anthropic 形态。
const upstreamStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"native","role":"assistant","usage":{"input_tokens":9}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}

event: message_stop
data: {"type":"message_stop"}

`

// requestBodies 是各入站协议一份最小的等价请求。
var requestBodies = map[string]string{
	codec.ProtocolAnthropic: `{"model":"user-model","max_tokens":64,
	  "messages":[{"role":"user","content":"hi"}]}`,
	codec.ProtocolChatCompletions: `{"model":"user-model","max_tokens":64,
	  "messages":[{"role":"user","content":"hi"}]}`,
	codec.ProtocolResponses: `{"model":"user-model","max_output_tokens":64,"input":"hi"}`,
}

// TestEveryPathAliasReachesTheRightInboundProtocol 是路径→协议的表驱动测试。
//
// 别名少一个就有一类客户端配不通，而这种问题只在真客户端接进来时才发现。
// 断言用的是「调度层收到的 inbound_protocol」：那是路径映射的唯一可观察结果。
func TestEveryPathAliasReachesTheRightInboundProtocol(t *testing.T) {
	cases := []struct {
		path     string
		protocol string
	}{
		{"/v1/messages", codec.ProtocolAnthropic},
		{"/v1/v1/messages", codec.ProtocolAnthropic},
		{"/anthropic/v1/messages", codec.ProtocolAnthropic},
		{"/messages", codec.ProtocolAnthropic},
		{"/v1/chat/completions", codec.ProtocolChatCompletions},
		{"/v1/v1/chat/completions", codec.ProtocolChatCompletions},
		{"/openai/v1/chat/completions", codec.ProtocolChatCompletions},
		{"/chat/completions", codec.ProtocolChatCompletions},
		{"/v1/responses", codec.ProtocolResponses},
		{"/v1/v1/responses", codec.ProtocolResponses},
		{"/openai/v1/responses", codec.ProtocolResponses},
		{"/responses", codec.ProtocolResponses},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			f := newFixture(t)
			resp := f.post(t, c.path, requestBodies[c.protocol], nil)
			if resp.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
			}
			disp := f.relay.Dispatches()
			if len(disp) != 1 {
				t.Fatalf("dispatches = %d, want 1", len(disp))
			}
			if disp[0].InboundProtocol != c.protocol {
				t.Errorf("inbound_protocol = %q, want %q", disp[0].InboundProtocol, c.protocol)
			}
			if disp[0].Model != "user-model" {
				t.Errorf("model = %q, want user-model", disp[0].Model)
			}
		})
	}
}

func TestGeminiHasNoInboundEndpoint(t *testing.T) {
	// gemini 只作为上游协议存在。开一个入站端点就得实现它的入站 codec，
	// 而没有客户端需要本服务扮演 gemini。
	//
	// /v1beta/models/... 回 405 而非 404：单模型查询占了这个前缀的 GET，
	// 于是 POST 变成方法不匹配。两个码都表示「这里没有对话端点」，而 405
	// 还带 Allow: GET，比 404 更能说明这个前缀上到底有什么。
	f := newFixture(t)
	cases := map[string]int{
		"/v1beta/models/x:generateContent": http.StatusMethodNotAllowed,
		"/gemini/v1/messages":              http.StatusNotFound,
	}
	for path, want := range cases {
		resp := f.post(t, path, `{}`, nil)
		if resp.Code != want {
			t.Errorf("%s: status = %d, want %d", path, resp.Code, want)
		}
		if len(f.relay.Dispatches()) != 0 {
			t.Errorf("%s: must not dispatch", path)
		}
	}
}

func TestClientKeyComesFromEitherHeader(t *testing.T) {
	// Anthropic 的 SDK 发 x-api-key，OpenAI 的发 Authorization。
	// 只认一个就有一半客户端过不了调度层的比对。
	cases := []struct {
		name   string
		header map[string]string
	}{
		{"x-api-key", map[string]string{"x-api-key": "ck-1"}},
		{"bearer", map[string]string{"Authorization": "Bearer ck-1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.post(t, "/v1/messages", requestBodies[codec.ProtocolAnthropic], c.header)
			disp := f.relay.Dispatches()
			if len(disp) != 1 || disp[0].ClientKey != "ck-1" {
				t.Fatalf("client_key = %+v, want ck-1 forwarded", disp)
			}
		})
	}
}

func TestRequestIDFromTheClientIsReused(t *testing.T) {
	// 沿用客户端的 id 才能把它的日志与本服务流水对上。
	f := newFixture(t)
	f.post(t, "/v1/messages", requestBodies[codec.ProtocolAnthropic],
		map[string]string{"X-Request-Id": "trace-abc"})
	disp := f.relay.Dispatches()
	if len(disp) != 1 || disp[0].RequestID != "trace-abc" {
		t.Fatalf("request_id = %+v, want trace-abc", disp)
	}
}

func TestRequestIDIsGeneratedWhenAbsent(t *testing.T) {
	f := newFixture(t)
	f.post(t, "/v1/messages", requestBodies[codec.ProtocolAnthropic], nil)
	disp := f.relay.Dispatches()
	if len(disp) != 1 || disp[0].RequestID == "" {
		t.Fatalf("request_id = %+v, want a generated id", disp)
	}
}

func TestMissingModelIsRejectedInTheClientProtocolShape(t *testing.T) {
	// 不带模型名就无从 dispatch。错误必须用客户端协议的形状，
	// 否则 SDK 会把它当成解码失败而不是业务错误。
	f := newFixture(t)
	resp := f.post(t, "/v1/messages", `{"max_tokens":64,"messages":[]}`, nil)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), `"type":"error"`) {
		t.Errorf("body must use the anthropic error shape: %s", resp.Body.String())
	}
	if len(f.relay.Dispatches()) != 0 {
		t.Error("a request without a model must not reach the dispatcher")
	}
}

func TestUndecodableBodyDoesNotReachTheDispatcher(t *testing.T) {
	f := newFixture(t)
	resp := f.post(t, "/v1/chat/completions", `{not json`, nil)
	if resp.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", resp.Code, resp.Body.String())
	}
	if len(f.relay.Dispatches()) != 0 {
		t.Error("an undecodable body must not reach the dispatcher")
	}
}

func TestCountTokensEstimatesLocallyWithoutTouchingUpstream(t *testing.T) {
	// 客户端调它只是想知道提示多长。为此 dispatch 一次并消耗上游配额不值得。
	f := newFixture(t)
	for _, path := range []string{
		"/v1/messages/count_tokens",
		"/anthropic/v1/messages/count_tokens",
		"/messages/count_tokens",
	} {
		t.Run(path, func(t *testing.T) {
			resp := f.post(t, path, `{"model":"user-model","messages":[
			  {"role":"user","content":"count these tokens please"}]}`, nil)
			if resp.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
			}
			var out struct {
				InputTokens int64 `json:"input_tokens"`
			}
			if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if out.InputTokens <= 0 {
				t.Errorf("input_tokens = %d, want a positive estimate", out.InputTokens)
			}
		})
	}
	if len(f.relay.Dispatches()) != 0 {
		t.Error("count_tokens must not dispatch")
	}
	if f.upstreamCalls() != 0 {
		t.Error("count_tokens must not call the upstream")
	}
}

func TestModelListShapeFollowsTheRequestPath(t *testing.T) {
	// 带族前缀的路径外形固定：SDK 解析不出自己认识的字段名会直接报错。
	// 不带族前缀的 /v1/models 与 /models 两族都会打，外形按请求头推断，
	// 那几条在 manifest_test.go 里。
	cases := []struct {
		path       string
		wantKeys   []string
		wantAbsent string
	}{
		{"/anthropic/v1/models", []string{`"display_name"`, `"has_more"`}, `"object":"list"`},
		{"/openai/v1/models", []string{`"object":"list"`, `"owned_by"`}, `"display_name"`},
		{"/v1beta/models", []string{`"models"`, `"supportedGenerationMethods"`}, `"object":"list"`},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			f := newFixture(t)
			resp := f.get(t, c.path)
			if resp.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
			}
			body := resp.Body.String()
			for _, key := range c.wantKeys {
				if !strings.Contains(body, key) {
					t.Errorf("body missing %s: %s", key, body)
				}
			}
			if strings.Contains(body, c.wantAbsent) {
				t.Errorf("body must not carry %s: %s", c.wantAbsent, body)
			}
			// 禁用的模型不列出：客户端会把清单当可选项展示。
			if strings.Contains(body, "disabled-model") {
				t.Errorf("disabled models must not be listed: %s", body)
			}
		})
	}
}

func TestNonStreamingClientGetsAJSONBodyEvenThoughUpstreamStreams(t *testing.T) {
	// 对上游一律流式，客户端要非流式时由数据面聚合。
	f := newFixture(t)
	resp := f.post(t, "/v1/messages", requestBodies[codec.ProtocolAnthropic], nil)
	if got := resp.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("content-type = %q, want json", got)
	}
	var out struct {
		Content []struct{ Text string } `json:"content"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, resp.Body.String())
	}
	if len(out.Content) != 1 || out.Content[0].Text != "hello" {
		t.Errorf("content = %+v, want one block saying hello", out.Content)
	}
	// 上游那一侧必须是流式请求，否则数据面的单一解码路径不成立。
	if !f.upstreamSawStream() {
		t.Error("the upstream request must ask for streaming")
	}
}

func TestStreamingClientGetsSSE(t *testing.T) {
	f := newFixture(t)
	body := `{"model":"user-model","max_tokens":64,"stream":true,
	  "messages":[{"role":"user","content":"hi"}]}`
	resp := f.post(t, "/v1/messages", body, nil)
	if got := resp.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Errorf("content-type = %q, want text/event-stream", got)
	}
	if !strings.Contains(resp.Body.String(), "message_stop") {
		t.Errorf("stream must be terminated: %s", resp.Body.String())
	}
}

func TestHealthReportsServiceUnavailableWhenDegraded(t *testing.T) {
	// 编排器与反代按状态码判定，只看 body 的很少。
	f := newFixture(t)
	f.health = httpapi.Health{Status: "degraded", Database: "ok", Relay: "down"}
	resp := f.get(t, "/health")
	if resp.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503: %s", resp.Code, resp.Body.String())
	}

	f.health = httpapi.Health{Status: "ok", Database: "ok", Relay: "ok"}
	if resp := f.get(t, "/health"); resp.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
}

func TestWrongMethodIsRejected(t *testing.T) {
	// GET /v1/messages 只可能是配置搞错了；当成对话请求处理会读到空体。
	f := newFixture(t)
	resp := f.get(t, "/v1/messages")
	if resp.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.Code)
	}
}

// TestEveryDataPlanePathHasAVersionDoubledAlias 是源码级守卫。
//
// 加新端点时容易只加原有那几条别名，而漏了 /v1/v1——这种遗漏在真客户端
// 把 base_url 配成 host/v1 之前不会暴露。断言用行为而非读源码：
// 每条 /v1/ 别名都必须有一条 /v1/v1/ 的孪生路径同样可达。
func TestEveryDataPlanePathHasAVersionDoubledAlias(t *testing.T) {
	cases := []struct {
		method string
		v1     string
		body   string
	}{
		{http.MethodPost, "/v1/messages", requestBodies[codec.ProtocolAnthropic]},
		{http.MethodPost, "/v1/chat/completions", requestBodies[codec.ProtocolChatCompletions]},
		{http.MethodPost, "/v1/responses", requestBodies[codec.ProtocolResponses]},
		{http.MethodPost, "/v1/messages/count_tokens", requestBodies[codec.ProtocolAnthropic]},
		{http.MethodGet, "/v1/models", ""},
	}
	for _, c := range cases {
		doubled := strings.Replace(c.v1, "/v1/", "/v1/v1/", 1)
		t.Run(doubled, func(t *testing.T) {
			f := newFixture(t)
			var resp *httptest.ResponseRecorder
			if c.method == http.MethodGet {
				resp = f.get(t, doubled)
			} else {
				resp = f.post(t, doubled, c.body, nil)
			}
			if resp.Code != http.StatusOK {
				t.Fatalf("%s %s: status = %d, want 200 (every /v1/ path needs a /v1/v1/ twin): %s",
					c.method, doubled, resp.Code, resp.Body.String())
			}
		})
	}
}

// --- fixture ---

type fixture struct {
	handler http.Handler
	relay   *relaymock.Mock
	health  httpapi.Health
	records *recorderSpy

	upstream *upstreamSpy
}

// newFixture 装一台完整的服务。maxBody 传 0 表示用默认上限。
func newFixture(t *testing.T, maxBody ...int64) *fixture {
	return newFixtureWithTarget(t, nil, maxBody...)
}

// newFixtureWithTarget 同上，但可以改调度层给出的目标。
//
// 改目标而不是暴露 relaymock 的 steps：目标是在 mock 构造时定下的，
// 事后改字段要么得导出内部切片，要么在服务已经起来之后改共享状态。
func newFixtureWithTarget(t *testing.T, tune func(*relayclient.Target), maxBody ...int64) *fixture {
	return newFixtureTuned(t, tune, nil, maxBody...)
}

// newFixtureTuned 同上，另外可以改服务本身的配置。
func newFixtureTuned(t *testing.T, tune func(*relayclient.Target),
	tuneServer func(*httpapi.Server), maxBody ...int64) *fixture {
	return newFixtureSteps(t, 1, tune, tuneServer, maxBody...)
}

// newFixtureSteps 同上，但预设 steps 个 dispatch 答案。
//
// 默认只给一个：绝大多数用例只发一次请求，多给会掩盖「重试时又去要了一次目标」
// 这类问题。需要连发多次成功请求的用例才调这个。
func newFixtureSteps(t *testing.T, steps int, tune func(*relayclient.Target),
	tuneServer func(*httpapi.Server), maxBody ...int64) *fixture {
	t.Helper()

	spy := &upstreamSpy{}
	up := httptest.NewServer(spy)
	t.Cleanup(up.Close)

	target := relayclient.Target{
		ModelID:     "m-1",
		Account:     "acc-1",
		Protocol:    codec.ProtocolAnthropic,
		BaseURL:     up.URL,
		NativeModel: "native",
		Headers:     map[string]string{"x-api-key": "sk-upstream"},
	}
	if tune != nil {
		tune(&target)
	}
	preset := make([]relaymock.Step, steps)
	for i := range preset {
		preset[i] = relaymock.Step{Target: target}
	}
	relay := relaymock.New(preset...)
	relay.Models = []relayclient.UserModelSummary{
		{Name: "user-model", Collection: "main", Protocol: codec.ProtocolAnthropic, Enabled: true},
		{Name: "disabled-model", Collection: "main", Enabled: false},
	}
	relaySrv := relay.Start()
	t.Cleanup(relaySrv.Close)

	rec := &recorderSpy{}
	f := &fixture{relay: relay, upstream: spy, records: rec, health: httpapi.Health{Status: "ok"}}
	var limit int64
	if len(maxBody) > 0 {
		limit = maxBody[0]
	}
	srv := &httpapi.Server{
		Pipeline: &pipeline.Pipeline{
			Dispatch: relayclient.New(relaySrv.URL, ""),
			Recorder: rec,
			Opts: pipeline.Options{
				MaxAttempts:       2,
				FirstTokenTimeout: 2 * time.Second,
				IdleTimeout:       2 * time.Second,
			},
		},
		Models:       modelLister{relay: relayclient.New(relaySrv.URL, "")},
		Health:       healthFunc(func() httpapi.Health { return f.health }),
		Recorder:     rec,
		MaxBodyBytes: limit,
	}
	if tuneServer != nil {
		tuneServer(srv)
	}
	f.handler = srv.Handler()
	return f
}

func (f *fixture) post(t *testing.T, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

func (f *fixture) get(t *testing.T, path string) *httptest.ResponseRecorder {
	return f.getWith(t, path, nil)
}

// getWith 带头发 GET。清单外形按头推断，那几条用例必须能自定头。
func (f *fixture) getWith(t *testing.T, path string,
	headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

// do 发一个任意方法、任意头的无体请求。预检用它。
func (f *fixture) do(t *testing.T, method, path string,
	headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

// postRaw 发一个原始字节体，可自定方法与头。受理面的用例要靠它构造
// 压缩体、带 BOM 的体、超限体这些 post 表达不了的形态。
func (f *fixture) postRaw(t *testing.T, method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

func (f *fixture) upstreamCalls() int      { return f.upstream.count() }
func (f *fixture) upstreamSawStream() bool { return f.upstream.sawStream() }

// recorderSpy 收下所有流水。加锁：pipeline 的记流水可能发生在别的协程。
type recorderSpy struct {
	mu   sync.Mutex
	recs []pipeline.Record
}

func (r *recorderSpy) Record(rec pipeline.Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = append(r.recs, rec)
}

func (r *recorderSpy) all() []pipeline.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]pipeline.Record(nil), r.recs...)
}

// one 取唯一一条流水，条数不对就失败——「记了几条」本身是断言的一部分。
func (r *recorderSpy) one(t *testing.T) pipeline.Record {
	t.Helper()
	got := r.all()
	if len(got) != 1 {
		t.Fatalf("records = %d, want exactly 1: %+v", len(got), got)
	}
	return got[0]
}

// upstreamSpy 是假上游：记下收到的请求体并回一段固定的流。
// 字段加锁：处理函数跑在 httptest 的协程上，断言在测试协程读。
type upstreamSpy struct {
	mu      sync.Mutex
	calls   int
	stream  bool
	headers http.Header
}

func (s *upstreamSpy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Stream bool `json:"stream"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	s.calls++
	s.stream = body.Stream
	s.headers = r.Header.Clone()
	s.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(upstreamStream))
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}

func (s *upstreamSpy) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *upstreamSpy) sawStream() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream
}

func (s *upstreamSpy) header(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.headers.Get(name)
}

// hasHeader 与 header 分开：空字符串既可能是「没这个头」也可能是
// 「头存在但值为空」，断言「不该发出去」时必须能区分。
func (s *upstreamSpy) hasHeader(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.headers[http.CanonicalHeaderKey(name)]
	return ok
}

type modelLister struct{ relay *relayclient.Client }

func (m modelLister) List(ctx context.Context) ([]relayclient.UserModelSummary, error) {
	return m.relay.Models(ctx)
}

type healthFunc func() httpapi.Health

func (f healthFunc) Check(context.Context) httpapi.Health { return f() }
