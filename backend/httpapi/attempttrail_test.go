package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/contract/agentv1"
	"github.com/aceaura/model-surge-agent/backend/pipeline"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
)

func trailRecord() pipeline.Record {
	return pipeline.Record{
		RequestID: "req-trail", InboundProtocol: "anthropic", UserModel: "m",
		Outcome: relayclient.OutcomeNormal, Attempts: 2,
		DispatchMS: 7, UpstreamMS: 20,
		AttemptsTrail: []pipeline.AttemptRecord{
			{
				N: 1, ModelID: "kimi-1/k3", Account: "acc-a", OutboundProtocol: "anthropic",
				Outcome: relayclient.OutcomeRetrying, StatusCode: 429,
				DispatchMS: 3, UpstreamMS: 8,
				ErrorCode: "rate_limited", ErrorMessage: "slow down",
				RetryAfter: time.Now().Add(time.Minute).UTC().Truncate(time.Second),
			},
			{
				N: 2, ModelID: "ark-1/ds", Account: "acc-b", OutboundProtocol: "chat_completions",
				Outcome: relayclient.OutcomeNormal, StatusCode: 200,
				DispatchMS: 4, UpstreamMS: 12,
			},
		},
	}
}

// 详情端点出轨迹：detailOf 是逐字段手写的搬运，漏一项既不报错也不崩，
// 症状只是运维永远看不到那一列。
func TestRequestDetailCarriesAttemptsTrail(t *testing.T) {
	a := newAdmin(t)
	a.requests.records = []pipeline.Record{trailRecord()}

	rr := a.get(t, "/admin/requests/req-trail", adminKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body)
	}
	var got agentv1.RequestDetail
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal %s: %v", rr.Body, err)
	}
	if len(got.AttemptsTrail) != 2 {
		t.Fatalf("轨迹 = %d 项：%s", len(got.AttemptsTrail), rr.Body)
	}
	want := trailRecord().AttemptsTrail
	for i := range want {
		have := got.AttemptsTrail[i]
		if have.N != want[i].N || have.ModelID != want[i].ModelID ||
			have.Account != want[i].Account ||
			have.OutboundProtocol != want[i].OutboundProtocol ||
			have.Outcome != want[i].Outcome || have.StatusCode != want[i].StatusCode ||
			have.DispatchMS != want[i].DispatchMS || have.UpstreamMS != want[i].UpstreamMS ||
			have.ErrorCode != want[i].ErrorCode || have.ErrorMessage != want[i].ErrorMessage {
			t.Errorf("第 %d 项搬错了：\n want %+v\n have %+v", i+1, want[i], have)
		}
	}
	if !got.AttemptsTrail[0].RetryAfter.Equal(want[0].RetryAfter) {
		t.Errorf("retry_after = %v，要 %v", got.AttemptsTrail[0].RetryAfter, want[0].RetryAfter)
	}
	// 详情同时还得带着列表项的全部字段：内嵌 RequestSummary 一旦改成
	// 平铺又漏字段，前端详情页会整片空。
	if got.RequestID != "req-trail" || got.DispatchMS != 7 {
		t.Errorf("详情丢了列表项字段：%+v", got.RequestSummary)
	}
}

// 列表刻意不带轨迹：一页最多 200 条，每条再挂 N 项会让响应随重试次数膨胀。
func TestRequestListOmitsAttemptsTrail(t *testing.T) {
	a := newAdmin(t)
	a.requests.records = []pipeline.Record{trailRecord()}

	rr := a.get(t, "/admin/requests", adminKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body)
	}
	if strings.Contains(rr.Body.String(), "attempts_trail") {
		t.Errorf("列表带上了轨迹，响应会随重试次数膨胀：%s", rr.Body)
	}
	// 但列表仍要带 attempts 计数：它是「有没有重试过」的入口，
	// 看到它不为 1 才会去点详情。
	var page agentv1.RequestPage
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(page.Requests) != 1 || page.Requests[0].Attempts != 2 {
		t.Errorf("列表没带 attempts 计数：%s", rr.Body)
	}
}

// 没重试过的请求，详情里 attempts_trail 只有一项而不是整键消失。
func TestRequestDetailKeepsSingleAttemptTrail(t *testing.T) {
	a := newAdmin(t)
	a.requests.records = []pipeline.Record{{
		RequestID: "req-one", InboundProtocol: "anthropic", UserModel: "m",
		Outcome:       relayclient.OutcomeNormal,
		AttemptsTrail: []pipeline.AttemptRecord{{N: 1, ModelID: "kimi-1/k3"}},
	}}

	rr := a.get(t, "/admin/requests/req-one", adminKey)
	var got agentv1.RequestDetail
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal %s: %v", rr.Body, err)
	}
	if len(got.AttemptsTrail) != 1 || got.AttemptsTrail[0].N != 1 {
		t.Errorf("单次尝试的轨迹没出：%s", rr.Body)
	}
}
