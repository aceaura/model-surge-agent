package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/contract/agentv1"
	"github.com/aceaura/model-surge-agent/backend/httpapi"
	"github.com/aceaura/model-surge-agent/backend/pipeline"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
	"github.com/aceaura/model-surge-agent/backend/store"
)

const adminKey = "admin-key-1"

func TestAdminRejectsMissingOrWrongKey(t *testing.T) {
	// 管理面能看到全部流水与账号名，鉴权失效等于把运维视图公开。
	a := newAdmin(t)
	for _, name := range []string{"", "wrong-key"} {
		resp := a.get(t, "/admin/health", name)
		if resp.Code != http.StatusUnauthorized {
			t.Errorf("key %q: status = %d, want 401", name, resp.Code)
		}
		var env agentv1.ErrorEnvelope
		if err := json.Unmarshal(resp.Body.Bytes(), &env); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if env.Error.Code != agentv1.CodeUnauthorized {
			t.Errorf("code = %q, want unauthorized", env.Error.Code)
		}
	}
}

func TestAdminHealthAlwaysReturns200EvenWhenDegraded(t *testing.T) {
	// 管理面是拿来看状态的。把降级表达成 HTTP 错误会让前端分不清
	// 「服务降级」与「管理面自己不通」。
	a := newAdmin(t)
	a.health = httpapi.Health{Status: "degraded", Database: "down", Cache: "ok", Relay: "ok"}
	resp := a.get(t, "/admin/health", adminKey)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
	var h agentv1.Health
	if err := json.Unmarshal(resp.Body.Bytes(), &h); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if h.Status != "degraded" || h.Database != "down" {
		t.Errorf("health = %+v, want the degraded detail passed through", h)
	}
}

func TestAdminRequestsPaginatesWithAnOpaqueCursor(t *testing.T) {
	// 游标对前端不透明：内部换分页实现时不必同步改前端。
	a := newAdmin(t)
	a.requests.next = store.Cursor{At: time.Now().UTC(), RequestID: "req-9"}
	a.requests.records = []pipeline.Record{{
		RequestID: "req-9", At: time.Now().UTC(), InboundProtocol: codec.ProtocolAnthropic,
		UserModel: "user-model", Outcome: relayclient.OutcomeNormal,
	}}

	resp := a.get(t, "/admin/requests?limit=1", adminKey)
	var page agentv1.RequestPage
	if err := json.Unmarshal(resp.Body.Bytes(), &page); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, resp.Body.String())
	}
	if len(page.Requests) != 1 || page.Requests[0].RequestID != "req-9" {
		t.Fatalf("requests = %+v, want one req-9", page.Requests)
	}
	if page.NextCursor == "" {
		t.Fatal("next_cursor must be set when the store reports more")
	}
	if strings.Contains(page.NextCursor, "req-9") {
		t.Errorf("cursor must be opaque, not a readable key: %q", page.NextCursor)
	}

	// 回传游标必须还原成原来的 (at, request_id)。
	a.get(t, "/admin/requests?cursor="+page.NextCursor, adminKey)
	got := a.requests.lastFilter.Cursor
	if got.RequestID != "req-9" {
		t.Errorf("decoded cursor = %+v, want request_id req-9", got)
	}
}

func TestAdminRequestsRejectsAMalformedCursor(t *testing.T) {
	a := newAdmin(t)
	resp := a.get(t, "/admin/requests?cursor=not-a-cursor", adminKey)
	if resp.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", resp.Code, resp.Body.String())
	}
}

func TestAdminRequestsForwardsFilters(t *testing.T) {
	a := newAdmin(t)
	a.get(t, "/admin/requests?outcome=abnormal&model_id=m-1&user_model=um&limit=7", adminKey)
	f := a.requests.lastFilter
	if f.Outcome != "abnormal" || f.ModelID != "m-1" || f.UserModel != "um" || f.Limit != 7 {
		t.Errorf("filter = %+v, want all four forwarded", f)
	}
}

func TestAdminRequestDetailReports404ForUnknownID(t *testing.T) {
	a := newAdmin(t)
	a.requests.getErr = store.ErrNotFound
	resp := a.get(t, "/admin/requests/nope", adminKey)
	if resp.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: %s", resp.Code, resp.Body.String())
	}
}

func TestAdminReportsUnavailableWhenTheStoreIsMissing(t *testing.T) {
	// PG 挂了仍要能起服务并看健康状态，所以流水接口报 unavailable 而不是崩。
	a := newAdmin(t)
	a.admin.Requests = nil
	a.admin.Outbox = nil
	a.rebuild()
	for _, path := range []string{"/admin/requests", "/admin/requests/x", "/admin/outbox"} {
		resp := a.get(t, path, adminKey)
		if resp.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503", path, resp.Code)
		}
	}
}

func TestAdminLiveAndStatsDegradeToEmptyWhenCacheIsDown(t *testing.T) {
	// 缓存不可用时回空集加 degraded 标记，前端才能区分
	// 「没有流量」与「缓存挂了」。整页报错会把其余面板一起拖下水。
	a := newAdmin(t)

	resp := a.get(t, "/admin/live", adminKey)
	if resp.Code != http.StatusOK {
		t.Fatalf("live status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
	var live agentv1.LivePage
	if err := json.Unmarshal(resp.Body.Bytes(), &live); err != nil {
		t.Fatalf("unmarshal live: %v", err)
	}
	if len(live.Entries) != 0 {
		t.Errorf("entries = %+v, want empty", live.Entries)
	}
	if !live.Degraded {
		t.Error("live must be flagged degraded when the cache is unavailable")
	}

	resp = a.get(t, "/admin/stats", adminKey)
	if resp.Code != http.StatusOK {
		t.Fatalf("stats status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
	var stats agentv1.Stats
	if err := json.Unmarshal(resp.Body.Bytes(), &stats); err != nil {
		t.Fatalf("unmarshal stats: %v", err)
	}
	if !stats.Degraded {
		t.Error("stats must be flagged degraded when the cache is unavailable")
	}
	if stats.Window != "1h" {
		t.Errorf("window = %q, want the 1h default", stats.Window)
	}
}

func TestAdminStatsRejectsAnUnsupportedWindow(t *testing.T) {
	// 分钟桶只留 2 小时，更长的窗口读不到数据。给一张必然半空的图
	// 比直接拒绝更容易让人误判。
	a := newAdmin(t)
	if resp := a.get(t, "/admin/stats?window=7d", adminKey); resp.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", resp.Code, resp.Body.String())
	}
	for _, w := range []string{"1h", "24h"} {
		if resp := a.get(t, "/admin/stats?window="+w, adminKey); resp.Code != http.StatusOK {
			t.Errorf("window %s: status = %d, want 200", w, resp.Code)
		}
	}
}

func TestAdminOutboxListsAndRevives(t *testing.T) {
	a := newAdmin(t)
	a.outbox.entries = []store.Entry{{
		ReportID: "req-1:0",
		Report: relayclient.ResultReport{
			RequestID: "req-1", ModelID: "m-1", Outcome: relayclient.OutcomeNormal,
		},
		Attempts: 20, LastError: "relay unreachable",
	}}

	resp := a.get(t, "/admin/outbox?state=dead", adminKey)
	var page agentv1.OutboxPage
	if err := json.Unmarshal(resp.Body.Bytes(), &page); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, resp.Body.String())
	}
	if len(page.Entries) != 1 || page.Entries[0].ReportID != "req-1:0" {
		t.Fatalf("entries = %+v, want one req-1:0", page.Entries)
	}
	if a.outbox.lastState != store.StateDead {
		t.Errorf("state = %q, want dead forwarded to the store", a.outbox.lastState)
	}

	resp = a.post(t, "/admin/outbox/req-1:0/retry", adminKey)
	if resp.Code != http.StatusOK {
		t.Fatalf("retry status = %d: %s", resp.Code, resp.Body.String())
	}
	if a.outbox.revived != "req-1:0" {
		t.Errorf("revived = %q, want req-1:0", a.outbox.revived)
	}
}

func TestAdminOutboxRejectsAnUnknownState(t *testing.T) {
	a := newAdmin(t)
	if resp := a.get(t, "/admin/outbox?state=weird", adminKey); resp.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", resp.Code, resp.Body.String())
	}
}

func TestAdminOutboxRetryReports404ForUnknownReport(t *testing.T) {
	a := newAdmin(t)
	a.outbox.reviveErr = store.ErrNotFound
	if resp := a.post(t, "/admin/outbox/nope/retry", adminKey); resp.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: %s", resp.Code, resp.Body.String())
	}
}

func TestAdminModelsFlagsModelsWithNoOutboundCodec(t *testing.T) {
	// 调度层可能把请求路由到本服务没实现出站编解码的协议上，那必然以
	// invalid_model 收场。在管理面直接可见，免得等线上报错才发现。
	a := newAdmin(t)
	a.models = []relayclient.UserModelSummary{
		{Name: "ok-model", Protocol: codec.ProtocolAnthropic, Enabled: true},
		{Name: "orphan-model", Protocol: "some_future_protocol", Enabled: true},
	}
	resp := a.get(t, "/admin/models", adminKey)
	var page agentv1.ModelsPage
	if err := json.Unmarshal(resp.Body.Bytes(), &page); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, resp.Body.String())
	}
	ready := map[string]bool{}
	for _, m := range page.Models {
		ready[m.Name] = m.OutboundReady
	}
	if !ready["ok-model"] {
		t.Error("a model on a registered protocol must be marked ready")
	}
	if ready["orphan-model"] {
		t.Error("a model on an unimplemented protocol must not be marked ready")
	}
	// 装配了哪些协议一并给出，前端才能解释 OutboundReady 为假的原因。
	if len(page.Outbound) != 4 {
		t.Errorf("outbound = %v, want all four registered", page.Outbound)
	}
}

func TestAdminIsAbsentWhenNotConfigured(t *testing.T) {
	// 不配管理面就不该有这些路径，而不是一个必然 401 的入口。
	f := newFixture(t)
	if resp := f.get(t, "/admin/health"); resp.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.Code)
	}
}

// --- admin fixture ---

type adminFixture struct {
	handler  http.Handler
	admin    *httpapi.Admin
	requests *fakeRequests
	outbox   *fakeOutbox
	health   httpapi.Health
	models   []relayclient.UserModelSummary
}

func newAdmin(t *testing.T) *adminFixture {
	t.Helper()
	a := &adminFixture{
		requests: &fakeRequests{},
		outbox:   &fakeOutbox{},
		health:   httpapi.Health{Status: "ok"},
		models:   []relayclient.UserModelSummary{{Name: "user-model", Enabled: true}},
	}
	a.admin = &httpapi.Admin{
		Key:      adminKey,
		Requests: a.requests,
		Outbox:   a.outbox,
		// Cache 为 nil：缓存未配置与连不上走同一条降级路径，
		// 所以 nil 就能测降级，不必起一个真 Redis。
		Cache:  nil,
		Models: listerFunc(func() []relayclient.UserModelSummary { return a.models }),
		Health: healthFunc(func() httpapi.Health { return a.health }),
	}
	a.rebuild()
	return a
}

func (a *adminFixture) rebuild() { a.handler = a.admin.Handler() }

func (a *adminFixture) get(t *testing.T, path, key string) *httptest.ResponseRecorder {
	t.Helper()
	return a.do(t, http.MethodGet, path, key)
}

func (a *adminFixture) post(t *testing.T, path, key string) *httptest.ResponseRecorder {
	t.Helper()
	return a.do(t, http.MethodPost, path, key)
}

func (a *adminFixture) do(t *testing.T, method, path, key string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	a.handler.ServeHTTP(w, r)
	return w
}

type fakeRequests struct {
	records    []pipeline.Record
	next       store.Cursor
	lastFilter store.Filter
	getErr     error
}

func (f *fakeRequests) List(_ context.Context, filter store.Filter) ([]pipeline.Record, store.Cursor, error) {
	f.lastFilter = filter
	return f.records, f.next, nil
}

func (f *fakeRequests) Get(_ context.Context, requestID string) (pipeline.Record, error) {
	if f.getErr != nil {
		return pipeline.Record{}, f.getErr
	}
	for _, rec := range f.records {
		if rec.RequestID == requestID {
			return rec, nil
		}
	}
	return pipeline.Record{}, store.ErrNotFound
}

type fakeOutbox struct {
	entries   []store.Entry
	lastState store.State
	revived   string
	reviveErr error
}

func (f *fakeOutbox) List(_ context.Context, state store.State, _ int) ([]store.Entry, error) {
	f.lastState = state
	return f.entries, nil
}

func (f *fakeOutbox) Revive(_ context.Context, reportID string) error {
	if f.reviveErr != nil {
		return f.reviveErr
	}
	f.revived = reportID
	return nil
}

type listerFunc func() []relayclient.UserModelSummary

func (f listerFunc) List(context.Context) ([]relayclient.UserModelSummary, error) {
	return f(), nil
}
