package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/contract/agentv1"
	"github.com/aceaura/model-surge-agent/backend/httpapi"
	"github.com/aceaura/model-surge-agent/backend/pipeline"
	"github.com/aceaura/model-surge-agent/backend/store"
)

// 本文件守健康检查里的饱和度信号。
//
// 这一段是「整个服务都慢、但每条请求日志都正常」时唯一能看的地方：
// 慢的那段在拿连接上，而那段不在任何一条请求的计时里。

// 没有 PG 时 pool 整个不出现，而不是报一组零。
//
// 报零会让运维看到 total=0、max=0，以为池配崩了去查 DSN，
// 而真相是这个部署根本没配 PG。零是「池此刻空闲」的合法状态。
func TestHealthOmitsPoolWhenNoDatabase(t *testing.T) {
	c := httpapi.Checker{}
	h := c.Check(context.Background())

	if h.Pool != nil {
		t.Fatalf("没配 PG 却报了池指标 %+v：零与「没有池」不是一回事", *h.Pool)
	}
	raw, err := json.Marshal(agentv1.Health(h))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), `"pool"`) {
		t.Errorf("JSON 里出现了 pool 键：%s", raw)
	}
}

// goroutine 数永远要有，且刻意不带 omitempty——NumGoroutine() 永远 >= 1。
func TestHealthAlwaysReportsGoroutines(t *testing.T) {
	c := httpapi.Checker{}
	h := c.Check(context.Background())

	if h.Goroutines < 1 {
		t.Fatalf("goroutines = %d，至少要 1（当前这个测试就占着一个）", h.Goroutines)
	}
	raw, err := json.Marshal(agentv1.Health(h))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"goroutines"`) {
		t.Errorf("JSON 里没有 goroutines 键（加了 omitempty？）：%s", raw)
	}
}

// 有 PG 时五个数都要报出来，并经 /admin/health 出到线上。
func TestHealthReportsPoolWhenDatabaseUp(t *testing.T) {
	dsn := os.Getenv("TEST_PG_DSN")
	if dsn == "" {
		t.Skip("TEST_PG_DSN not set")
	}
	db, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	c := httpapi.Checker{Store: db}
	h := c.Check(context.Background())

	if h.Database != "ok" {
		t.Fatalf("database = %q，测试库应当通", h.Database)
	}
	if h.Pool == nil {
		t.Fatal("PG 通了却没有池指标：整个服务变慢时这是唯一能看的地方")
	}
	if h.Pool.Max <= 0 {
		t.Errorf("pool.max = %d，必须是正数——零说明字段没被填上", h.Pool.Max)
	}
}

// 池指标要真的经 JSON 出到 /health，不是只在结构体里存在。
func TestHealthEndpointCarriesPoolAndGoroutines(t *testing.T) {
	waiting := int64(7)
	h := httpapi.Health{
		Status: "ok", Database: "ok", Cache: "ok", Relay: "ok",
		Goroutines: 42,
		// 五个值各不相同：任意两个字段互换都能被这组断言抓到。
		Pool: &agentv1.PoolStats{
			Acquired: 1, Idle: 2, Total: 3, Max: 4, AcquireWaiting: waiting,
		},
	}
	srv := &httpapi.Server{Health: healthFunc(func() httpapi.Health { return h })}

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body)
	}

	var got agentv1.Health
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal %s: %v", rr.Body, err)
	}
	if got.Goroutines != 42 {
		t.Errorf("goroutines = %d，要 42", got.Goroutines)
	}
	if got.Pool == nil {
		t.Fatalf("响应里没有 pool：%s", rr.Body)
	}
	if got.Pool.Acquired != 1 || got.Pool.Idle != 2 ||
		got.Pool.Total != 3 || got.Pool.Max != 4 {
		t.Errorf("pool 四个数 = %+v，要 (1,2,3,4)——错位会让运维读反饱和度", *got.Pool)
	}
	if got.Pool.AcquireWaiting != waiting {
		t.Errorf("acquire_waiting = %d，要 %d", got.Pool.AcquireWaiting, waiting)
	}
}

// 两段要经 summaryOf 出到 /admin/requests。
//
// summaryOf 是逐字段手写的搬运，漏一个字段既不报错也不崩，
// 症状只是前端那两列永远是空的。
func TestRequestSummaryCarriesLatencySegments(t *testing.T) {
	a := newAdmin(t)
	a.requests.records = []pipeline.Record{{
		RequestID: "req-seg", InboundProtocol: "anthropic", UserModel: "m",
		Outcome: "normal", LatencyMS: 900, DispatchMS: 111, UpstreamMS: 222,
	}}

	rr := a.get(t, "/admin/requests", adminKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body)
	}
	var page agentv1.RequestPage
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatalf("unmarshal %s: %v", rr.Body, err)
	}
	if len(page.Requests) != 1 {
		t.Fatalf("出 %d 条，要 1 条", len(page.Requests))
	}
	got := page.Requests[0]
	if got.DispatchMS != 111 {
		t.Errorf("dispatch_ms = %d，要 111（读到 222 说明 summaryOf 里两段搬反了）",
			got.DispatchMS)
	}
	if got.UpstreamMS != 222 {
		t.Errorf("upstream_ms = %d，要 222（读到 111 说明 summaryOf 里两段搬反了）",
			got.UpstreamMS)
	}
	if got.LatencyMS != 900 {
		t.Errorf("latency_ms = %d，要 900", got.LatencyMS)
	}
}

// 池已关闭时同样不报池指标。
//
// Store 为 nil 走的是 nil 判断那一支，测不到「Ping 失败」这一支——
// 而那一支才是真实故障形态：PG 挂了，池对象还在，Stat() 还能返回
// 最后一刻的残留数字，报出去会让运维以为池还活着。
func TestHealthOmitsPoolWhenDatabaseDown(t *testing.T) {
	dsn := os.Getenv("TEST_PG_DSN")
	if dsn == "" {
		t.Skip("TEST_PG_DSN not set")
	}
	db, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// 关掉池：此后 Ping 必失败，但 Stat() 仍会返回数字。
	db.Close()

	h := httpapi.Checker{Store: db}.Check(context.Background())
	if h.Database != "down" {
		t.Fatalf("database = %q，池已关闭应当报 down", h.Database)
	}
	if h.Pool != nil {
		t.Errorf("池已关闭却报了 %+v：那是最后一刻的残留，"+
			"报出去会让运维以为池还活着", *h.Pool)
	}
}

// Goroutines 为零值时 JSON 里也必须有这个键。
//
// 上面那个测试给的是 42，非零值在 omitempty 下照样会出现——
// 探针实测给 Goroutines 加上 omitempty 完全测不出来（变异 M19 未被检出）。
// 要钉住「刻意不加 omitempty」这个决定，断言必须落在零值上。
func TestGoroutinesKeyPresentEvenAtZero(t *testing.T) {
	raw, err := json.Marshal(agentv1.Health{Status: "ok"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"goroutines"`) {
		t.Errorf("零值时 goroutines 键消失了（加了 omitempty）：%s —— "+
			"NumGoroutine() 永远 >= 1，零值只会在字段没被填时出现，"+
			"那正是要暴露的 bug", raw)
	}
}
