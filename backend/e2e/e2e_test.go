// Package e2e_test 把整套装配跑起来：真 httpapi 路由 + 真 pipeline + 真 outbox
// worker + relaymock + 假上游。
//
// 各层的单测已经覆盖了各自的分支，这里只测「装配起来还成立」的那些性质——
// 单测里被 fake 掉的接缝在真装配下容易反过来：路由把协议认对了但 recorder
// 没接上、pipeline 判出 outcome 但没走到 outbox 落库、参数覆盖在 pipeline 里
// 有测但装配时漏传 Target 字段。这类问题只有整条链路一起跑才暴露。
package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/contract/agentv1"
	"github.com/aceaura/model-surge-agent/backend/httpapi"
	"github.com/aceaura/model-surge-agent/backend/outbox"
	"github.com/aceaura/model-surge-agent/backend/pipeline"
	"github.com/aceaura/model-surge-agent/backend/recorder"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
	"github.com/aceaura/model-surge-agent/backend/relaymock"
	"github.com/aceaura/model-surge-agent/backend/store"

	// 与 cmd/server 同样的空导入：装配的一部分就是这四个协议进注册表。
	_ "github.com/aceaura/model-surge-agent/backend/codec/anthropic"
	_ "github.com/aceaura/model-surge-agent/backend/codec/chatcompletions"
	_ "github.com/aceaura/model-surge-agent/backend/codec/gemini"
	_ "github.com/aceaura/model-surge-agent/backend/codec/responses"
)

const adminKey = "admin-key-e2e"

func TestHappyPathReportsNormalAndLandsInTheAdminAPI(t *testing.T) {
	h := newHarness(t, harnessOpts{
		targets: []relayclient.Target{anthropicTarget("m-a")},
		replies: []reply{{sse: goodStream}},
	})

	resp := h.post(t, "/v1/messages", anthropicRequest(false), map[string]string{"x-api-key": "ck-1"})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
	}

	reports := h.relay.Reports()
	if len(reports) != 1 || reports[0].Outcome != relayclient.OutcomeNormal {
		t.Fatalf("reports = %+v, want one normal", reports)
	}
	if reports[0].Usage.OutputTokens == 0 {
		t.Errorf("usage must reach the relay: %+v", reports[0].Usage)
	}

	// recorder 接上了才能在管理面看到这次调用。单测里 recorder 是 fake，
	// 装配漏接的话数据面照样工作，只是流水永远是空的。
	page := h.adminRequests(t, "")
	if len(page.Requests) != 1 {
		t.Fatalf("admin requests = %d, want 1", len(page.Requests))
	}
	got := page.Requests[0]
	if got.InboundProtocol != codec.ProtocolAnthropic || got.OutboundProtocol != codec.ProtocolAnthropic {
		t.Errorf("protocols = %q → %q", got.InboundProtocol, got.OutboundProtocol)
	}
	if got.Outcome != relayclient.OutcomeNormal || got.StatusCode != http.StatusOK {
		t.Errorf("outcome = %q status = %d", got.Outcome, got.StatusCode)
	}
	// 凭据绝不入库：管理面是给运维看的，泄一次就等于泄给所有能读它的人。
	if raw := h.adminRaw(t, "/admin/requests"); strings.Contains(raw, "sk-upstream") ||
		strings.Contains(raw, "ck-1") {
		t.Errorf("admin payload leaked a credential: %s", raw)
	}
}

func TestFailingTargetIsSwappedAndTriedIDsAccumulate(t *testing.T) {
	h := newHarness(t, harnessOpts{
		targets: []relayclient.Target{anthropicTarget("m-a"), anthropicTarget("m-b")},
		// 第一个目标 500：还没出任何帧，可以换。
		replies: []reply{{status: http.StatusInternalServerError, body: `{"error":{"message":"boom"}}`},
			{sse: goodStream}},
	})

	resp := h.post(t, "/v1/messages", anthropicRequest(false), nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
	}

	disp := h.relay.Dispatches()
	if len(disp) != 2 {
		t.Fatalf("dispatches = %d, want 2", len(disp))
	}
	if len(disp[0].TriedIDs) != 0 {
		t.Errorf("first dispatch must not carry tried_ids: %v", disp[0].TriedIDs)
	}
	// 换目标靠 tried_ids 而非租约：漏传就会反复选到同一个坏目标。
	if len(disp[1].TriedIDs) != 1 || disp[1].TriedIDs[0] != "m-a" {
		t.Errorf("second dispatch tried_ids = %v, want [m-a]", disp[1].TriedIDs)
	}

	reports := h.relay.Reports()
	if len(reports) != 2 {
		t.Fatalf("reports = %+v, want two", reports)
	}
	if reports[0].Outcome != relayclient.OutcomeRetrying {
		t.Errorf("first outcome = %q, want retrying", reports[0].Outcome)
	}
	if reports[1].Outcome != relayclient.OutcomeNormal {
		t.Errorf("second outcome = %q, want normal", reports[1].Outcome)
	}
	// report_id 带 attempt：两次尝试各记一笔，重放不会重复计数。
	if reports[0].ReportID == reports[1].ReportID {
		t.Errorf("report ids must differ per attempt: %q", reports[0].ReportID)
	}
}

func TestContextExceededDoesNotSwapTargets(t *testing.T) {
	// 输入太长是客户端的问题，换目标只是把同一个错再犯一遍，
	// 而 context_exceeded 这个 outcome 存在的意义就是别让它记成目标的失败。
	h := newHarness(t, harnessOpts{
		targets: []relayclient.Target{anthropicTarget("m-a"), anthropicTarget("m-b")},
		replies: []reply{{status: http.StatusBadRequest,
			body: `{"error":{"type":"invalid_request_error","message":"prompt is too long: 900000 tokens > 200000 maximum"}}`}},
	})

	resp := h.post(t, "/v1/messages", anthropicRequest(false), nil)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.Code, resp.Body.String())
	}
	if n := len(h.relay.Dispatches()); n != 1 {
		t.Errorf("dispatches = %d, want 1", n)
	}
	reports := h.relay.Reports()
	if len(reports) != 1 || reports[0].Outcome != relayclient.OutcomeContextExceeded {
		t.Fatalf("reports = %+v, want one context_exceeded", reports)
	}
}

func TestBrokenStreamAfterCommitStaysA200WithTheErrorInside(t *testing.T) {
	// 首帧已解码出事件，200 与响应头都写出去了。此后换目标会让客户端看到
	// 两段拼接的回答，改状态码更是不可能，所以只能把错误放进流里当终止。
	h := newHarness(t, harnessOpts{
		targets: []relayclient.Target{anthropicTarget("m-a"), anthropicTarget("m-b")},
		replies: []reply{{sse: truncatedStream, abort: true}},
	})

	resp := h.post(t, "/v1/messages", anthropicRequest(true), nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Errorf("stream must carry the error: %s", body)
	}
	// error 事件即终止。补 message_stop 会让客户端把残缺回答当正常结束。
	if strings.Contains(body, "message_stop") {
		t.Errorf("an error-terminated stream must not carry a normal terminator: %s", body)
	}
	if n := len(h.relay.Dispatches()); n != 1 {
		t.Errorf("dispatches = %d, want 1: committed 之后不该换目标", n)
	}
	reports := h.relay.Reports()
	if len(reports) != 1 || reports[0].Outcome != relayclient.OutcomeAbnormal {
		t.Fatalf("reports = %+v, want one abnormal", reports)
	}
}

func TestParamLayersLandOnTheWireBody(t *testing.T) {
	target := anthropicTarget("m-a")
	target.Defaults = json.RawMessage(`{"temperature":0.5,"top_p":0.9}`)
	target.Overrides = json.RawMessage(`{"temperature":0.2}`)

	h := newHarness(t, harnessOpts{
		targets: []relayclient.Target{target},
		replies: []reply{{sse: goodStream}},
	})

	// 客户端显式传了 temperature，没传 top_p。
	body := `{"model":"user-model","max_tokens":64,"temperature":0.7,
	  "messages":[{"role":"user","content":"hi"}]}`
	if resp := h.post(t, "/v1/messages", body, nil); resp.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
	}

	sent := h.upstream.bodies()
	if len(sent) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(sent))
	}
	var wire map[string]any
	if err := json.Unmarshal(sent[0], &wire); err != nil {
		t.Fatalf("unmarshal wire body: %v: %s", err, sent[0])
	}
	// overrides 无条件压盖，连客户端的显式取值也压。
	if wire["temperature"] != 0.2 {
		t.Errorf("temperature = %v, want 0.2 (override)", wire["temperature"])
	}
	// defaults 只填空缺。
	if wire["top_p"] != 0.9 {
		t.Errorf("top_p = %v, want 0.9 (default)", wire["top_p"])
	}
	// native model 必须替换掉 user model，否则上游不认这个名字。
	if wire["model"] != "native" {
		t.Errorf("model = %v, want native", wire["model"])
	}
}

func TestCrossProtocolInboundReachesGeminiOutbound(t *testing.T) {
	// 三入站 × 四出站的语义保真属于 codec 矩阵。这里只验装配：
	// 出站是 gemini 时，端点形态与出站协议确实按 target 走，不是按入站协议猜的。
	for _, inbound := range []struct {
		path string
		body string
	}{
		{"/v1/messages", anthropicRequest(false)},
		{"/v1/chat/completions", `{"model":"user-model","max_tokens":64,
		  "messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/responses", `{"model":"user-model","max_output_tokens":64,"input":"hi"}`},
	} {
		t.Run(inbound.path, func(t *testing.T) {
			h := newHarness(t, harnessOpts{
				targets: []relayclient.Target{geminiTarget("m-g")},
				replies: []reply{{sse: geminiStream}},
			})
			resp := h.post(t, inbound.path, inbound.body, nil)
			if resp.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
			}
			// gemini 的流式选择在端点上：模型名进路径、方法名换成 streamGenerateContent。
			url := h.upstream.urls()[0]
			if !strings.Contains(url, "/models/native:streamGenerateContent") ||
				!strings.Contains(url, "alt=sse") {
				t.Errorf("upstream url = %q", url)
			}
			page := h.adminRequests(t, "")
			if len(page.Requests) != 1 || page.Requests[0].OutboundProtocol != codec.ProtocolGemini {
				t.Errorf("outbound protocol not recorded as gemini: %+v", page.Requests)
			}
		})
	}
}

func TestFailedReportIsQueuedThenReplayedAndTheAdminCanSeeIt(t *testing.T) {
	h := newHarness(t, harnessOpts{
		targets: []relayclient.Target{anthropicTarget("m-a")},
		replies: []reply{{sse: goodStream}},
	})
	// 上报打不通时直报会失败。丢掉它就等于让调度层的用量与冷却失真，
	// 所以必须落库排队。
	h.relay.FailReports(&relayclient.Error{Code: relayclient.CodeInternal,
		Message: "relay down", Retryable: true})

	if resp := h.post(t, "/v1/messages", anthropicRequest(false), nil); resp.Code != http.StatusOK {
		t.Fatalf("客户端不该因为上报失败而受影响: status = %d", resp.Code)
	}
	if got := h.relay.Reports(); len(got) != 0 {
		t.Fatalf("reports = %+v, want none accepted yet", got)
	}

	ctx := context.Background()
	pending, dead, err := h.queue.Counts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if pending != 1 || dead != 0 {
		t.Fatalf("pending = %d dead = %d, want 1/0", pending, dead)
	}
	// 管理面能看到积压：用量缺口的排查从这里开始。
	var page agentv1.OutboxPage
	h.adminJSON(t, "/admin/outbox?state=pending", &page)
	if len(page.Entries) != 1 || page.Entries[0].LastError == "" {
		t.Errorf("admin outbox = %+v, want one entry with last_error", page.Entries)
	}

	h.relay.FailReports(nil)
	if sent := h.worker.Drain(ctx); sent != 1 {
		t.Fatalf("drained = %d, want 1", sent)
	}
	got := h.relay.Reports()
	if len(got) != 1 || got[0].Outcome != relayclient.OutcomeNormal {
		t.Fatalf("replayed reports = %+v", got)
	}
	// 送达即出队，否则会被无限重放。
	if pending, _, err := h.queue.Counts(ctx); err != nil || pending != 0 {
		t.Errorf("pending = %d (err %v), want 0", pending, err)
	}
}

func TestHealthIsDegradedWhenTheRelayIsUnreachable(t *testing.T) {
	// relay 不可达时数据面无法选目标，编排器必须能从状态码看出来。
	h := newHarness(t, harnessOpts{
		targets: []relayclient.Target{anthropicTarget("m-a")},
		replies: []reply{{sse: goodStream}},
	})
	h.relaySrv.Close()

	resp := h.get(t, "/health", nil)
	if resp.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503: %s", resp.Code, resp.Body.String())
	}
	var health agentv1.Health
	h.adminJSON(t, "/admin/health", &health)
	if health.Status != "degraded" || health.Relay == "ok" {
		t.Errorf("admin health = %+v", health)
	}
}

// --- harness ---

type harnessOpts struct {
	// targets 按顺序作为 dispatch 的答案。
	targets []relayclient.Target
	// replies 按顺序作为假上游的答案。
	replies []reply
}

type harness struct {
	handler  http.Handler
	relay    *relaymock.Mock
	relaySrv *httptest.Server
	upstream *fakeUpstream
	worker   *outbox.Worker
	queue    *store.Outbox
}

func newHarness(t *testing.T, opts harnessOpts) *harness {
	t.Helper()
	db := openStore(t)

	up := &fakeUpstream{replies: opts.replies}
	upSrv := httptest.NewServer(up)
	t.Cleanup(upSrv.Close)

	steps := make([]relaymock.Step, 0, len(opts.targets))
	for _, target := range opts.targets {
		target.BaseURL = upSrv.URL
		steps = append(steps, relaymock.Step{Target: target})
	}
	mock := relaymock.New(steps...)
	mock.Models = []relayclient.UserModelSummary{
		{Name: "user-model", Collection: "main", Protocol: codec.ProtocolAnthropic, Enabled: true},
	}
	relaySrv := mock.Start()
	t.Cleanup(relaySrv.Close)

	relay := relayclient.New(relaySrv.URL, "")
	queue := store.NewOutbox(db.Pool())
	requests := store.NewRequestLog(db.Pool())

	worker := &outbox.Worker{
		Queue:  queue,
		Sender: relay,
		Opts:   outbox.Options{MaxAttempts: 5},
		// 时间往前拨，Drain 才看得到刚入队那条的退避时间已到。
		Now: func() time.Time { return time.Now().Add(time.Hour) },
	}

	// Cache 为 nil：Redis 的降级路径由 cache 包自己的测试覆盖，
	// 这里不引入一个非必需依赖来门控整套端到端。
	models := httpapi.CachedModels{Relay: relay}
	health := httpapi.Checker{Store: db, Outbox: queue, Relay: relay}

	srv := &httpapi.Server{
		Pipeline: &pipeline.Pipeline{
			Dispatch: relay,
			Reporter: worker,
			Recorder: &recorder.Recorder{Log: requests},
			Opts: pipeline.Options{
				MaxAttempts:       3,
				FirstTokenTimeout: 3 * time.Second,
				IdleTimeout:       3 * time.Second,
				EstimateUsage:     true,
			},
		},
		Models: models,
		Health: health,
		Admin: &httpapi.Admin{
			Key: adminKey, Requests: requests, Outbox: queue,
			Models: models, Health: health,
		},
	}

	return &harness{
		handler: srv.Handler(), relay: mock, relaySrv: relaySrv,
		upstream: up, worker: worker, queue: queue,
	}
}

// openStore 打开测试库并清空两张表。整套端到端都用真 PG：outbox 与流水
// 是这里要验的接缝，用内存替身就把接缝本身替掉了。
func openStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("TEST_PG_DSN")
	if dsn == "" {
		t.Skip("TEST_PG_DSN not set")
	}
	ctx := context.Background()
	db, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(db.Close)
	if _, err := db.Pool().Exec(ctx, "TRUNCATE request_log, report_outbox"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return db
}

func (h *harness) post(t *testing.T, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	return w
}

func (h *harness) get(t *testing.T, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	return w
}

func (h *harness) adminRaw(t *testing.T, path string) string {
	t.Helper()
	resp := h.get(t, path, map[string]string{"Authorization": "Bearer " + adminKey})
	if resp.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, resp.Code, resp.Body.String())
	}
	return resp.Body.String()
}

func (h *harness) adminJSON(t *testing.T, path string, out any) {
	t.Helper()
	raw := h.adminRaw(t, path)
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		t.Fatalf("unmarshal %s: %v: %s", path, err, raw)
	}
}

func (h *harness) adminRequests(t *testing.T, query string) agentv1.RequestPage {
	t.Helper()
	var page agentv1.RequestPage
	h.adminJSON(t, "/admin/requests"+query, &page)
	return page
}

// --- 假上游 ---

// reply 是假上游的一次答案。status 为 0 视作 200 并发 sse；
// abort 表示把 sse 发出去之后直接掐断连接。
type reply struct {
	status int
	body   string
	sse    string
	abort  bool
}

type fakeUpstream struct {
	replies []reply

	mu     sync.Mutex
	served int
	got    [][]byte
	url    []string
}

func (u *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw := make([]byte, 0, 512)
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		raw = append(raw, buf[:n]...)
		if err != nil {
			break
		}
	}

	u.mu.Lock()
	u.got = append(u.got, raw)
	u.url = append(u.url, r.URL.String())
	var rep reply
	if u.served < len(u.replies) {
		rep = u.replies[u.served]
	} else {
		rep = reply{status: http.StatusServiceUnavailable,
			body: fmt.Sprintf(`{"error":{"message":"fake upstream ran out of replies after %d"}}`, u.served)}
	}
	u.served++
	u.mu.Unlock()

	if rep.status != 0 && rep.status != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rep.status)
		_, _ = w.Write([]byte(rep.body))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(rep.sse))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if rep.abort {
		// 掐断连接而非正常收尾：数据面必须把它当成读错误，
		// 而不是「流正常结束了只是没有终止帧」。
		panic(http.ErrAbortHandler)
	}
}

func (u *fakeUpstream) bodies() [][]byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([][]byte(nil), u.got...)
}

func (u *fakeUpstream) urls() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.url...)
}

// --- 固定数据 ---

func anthropicTarget(modelID string) relayclient.Target {
	return relayclient.Target{
		ModelID:     modelID,
		Account:     "acc-1",
		Protocol:    codec.ProtocolAnthropic,
		NativeModel: "native",
		Headers:     map[string]string{"x-api-key": "sk-upstream"},
	}
}

func geminiTarget(modelID string) relayclient.Target {
	return relayclient.Target{
		ModelID:     modelID,
		Account:     "acc-g",
		Protocol:    codec.ProtocolGemini,
		NativeModel: "native",
		Headers:     map[string]string{"x-goog-api-key": "sk-upstream"},
	}
}

func anthropicRequest(stream bool) string {
	return fmt.Sprintf(`{"model":"user-model","max_tokens":64,"stream":%v,
	  "messages":[{"role":"user","content":"hi"}]}`, stream)
}

const goodStream = `event: message_start
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

// truncatedStream 出了首帧内容就没了，配合 abort 模拟中途断流。
const truncatedStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"native","role":"assistant","usage":{"input_tokens":9}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hel"}}

`

const geminiStream = `data: {"responseId":"msg_1","modelVersion":"native","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"hello"}]}}],"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":3}}

data: {"candidates":[{"index":0,"finishReason":"STOP"}]}

`
