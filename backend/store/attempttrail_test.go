package store

import (
	"context"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/pipeline"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
)

// 轨迹整条往返：三项、逐项字段、顺序都要原样回来。
func TestAttemptsTrailRoundTrips(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	log := NewRequestLog(s.Pool())

	due := time.Now().Add(30 * time.Second).UTC().Truncate(time.Millisecond)
	rec := pipeline.Record{
		RequestID:       "req-trail-1",
		At:              time.Now(),
		InboundProtocol: "anthropic",
		UserModel:       "kimi-k3",
		Outcome:         relayclient.OutcomeNormal,
		Attempts:        3,
		DispatchMS:      9,
		UpstreamMS:      30,
		AttemptsTrail: []pipeline.AttemptRecord{
			{
				N: 1, ModelID: "kimi-1/k3", Account: "acc-a", OutboundProtocol: "anthropic",
				Outcome: relayclient.OutcomeRetrying, StatusCode: 429,
				DispatchMS: 3, UpstreamMS: 10,
				ErrorCode: "rate_limited", ErrorMessage: "slow down", RetryAfter: due,
			},
			{
				N: 2, ModelID: "ark-1/ds", Account: "acc-b", OutboundProtocol: "chat_completions",
				Outcome: relayclient.OutcomeRetrying, StatusCode: 500,
				DispatchMS: 4, UpstreamMS: 12,
				ErrorCode: "upstream_error", ErrorMessage: "boom",
			},
			{
				N: 3, ModelID: "kimi-2/k3", Account: "acc-c", OutboundProtocol: "anthropic",
				Outcome: relayclient.OutcomeNormal, StatusCode: 200,
				DispatchMS: 2, UpstreamMS: 8,
			},
		},
	}
	if err := log.Insert(ctx, rec); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := log.Get(ctx, rec.RequestID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.AttemptsTrail) != 3 {
		t.Fatalf("轨迹 = %d 项：%+v", len(got.AttemptsTrail), got.AttemptsTrail)
	}
	for i := range rec.AttemptsTrail {
		want, have := rec.AttemptsTrail[i], got.AttemptsTrail[i]
		if want.N != have.N || want.ModelID != have.ModelID || want.Account != have.Account ||
			want.OutboundProtocol != have.OutboundProtocol || want.Outcome != have.Outcome ||
			want.StatusCode != have.StatusCode || want.DispatchMS != have.DispatchMS ||
			want.UpstreamMS != have.UpstreamMS || want.ErrorCode != have.ErrorCode ||
			want.ErrorMessage != have.ErrorMessage {
			t.Errorf("第 %d 项往返后变了：\n want %+v\n have %+v", i+1, want, have)
		}
	}
	// retry_after 是唯一带时区的字段，单独按时刻比。
	if !got.AttemptsTrail[0].RetryAfter.Equal(due) {
		t.Errorf("retry_after 往返后 = %v，要 %v", got.AttemptsTrail[0].RetryAfter, due)
	}
	if !got.AttemptsTrail[2].RetryAfter.IsZero() {
		t.Errorf("上游没说到期时刻，往返后却有值：%v", got.AttemptsTrail[2].RetryAfter)
	}
}

// 空轨迹必须写成 []，不是 null。
//
// JSONB 列声明 NOT NULL，写 Go 的 nil 切片会被编成 null 而整条 INSERT 失败——
// 流水从此一条都记不下，而错误只在运行时出现。
func TestEmptyAttemptsTrailWritesAsArray(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	log := NewRequestLog(s.Pool())

	rec := pipeline.Record{
		RequestID: "req-trail-empty", At: time.Now(),
		InboundProtocol: "anthropic", UserModel: "kimi-k3",
		Outcome: relayclient.OutcomeNormal,
	}
	if err := log.Insert(ctx, rec); err != nil {
		t.Fatalf("nil 轨迹让整条流水写不进去：%v", err)
	}

	var raw string
	if err := s.Pool().QueryRow(ctx,
		"SELECT attempts_trail::text FROM request_log WHERE request_id = $1",
		rec.RequestID).Scan(&raw); err != nil {
		t.Fatalf("query: %v", err)
	}
	if raw != "[]" {
		t.Errorf("空轨迹在库里是 %q，要 %q", raw, "[]")
	}
}

// 同 request_id 重写时轨迹跟着更新：漏进 DO UPDATE SET 的列会一直留着首次的值。
func TestAttemptsTrailIsUpdatedOnConflict(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	log := NewRequestLog(s.Pool())

	base := pipeline.Record{
		RequestID: "req-trail-conflict", At: time.Now(),
		InboundProtocol: "anthropic", UserModel: "kimi-k3",
		Outcome:       relayclient.OutcomeNormal,
		AttemptsTrail: []pipeline.AttemptRecord{{N: 1, Account: "acc-first"}},
	}
	if err := log.Insert(ctx, base); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	base.AttemptsTrail = []pipeline.AttemptRecord{
		{N: 1, Account: "acc-second"}, {N: 2, Account: "acc-third"},
	}
	if err := log.Insert(ctx, base); err != nil {
		t.Fatalf("second insert: %v", err)
	}

	got, err := log.Get(ctx, base.RequestID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.AttemptsTrail) != 2 || got.AttemptsTrail[0].Account != "acc-second" {
		t.Errorf("重写后轨迹还是旧的，这一列漏进了 DO UPDATE：%+v", got.AttemptsTrail)
	}
}

// 老库补列：先把列删掉再重跑 migrate，列必须回来。
func TestMigrateAddsAttemptsTrailColumn(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	if _, err := s.Pool().Exec(ctx,
		"ALTER TABLE request_log DROP COLUMN IF EXISTS attempts_trail"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var exists bool
	err := s.Pool().QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		 WHERE table_name = 'request_log' AND column_name = 'attempts_trail')`).Scan(&exists)
	if err != nil {
		t.Fatalf("query column: %v", err)
	}
	if !exists {
		t.Fatal("attempts_trail 没被补回来：老库升级后 INSERT 会整条失败，" +
			"流水从此一条都记不下")
	}
	// 补回来的列还要真能写：DEFAULT 缺失会让 NOT NULL 拦住既有行。
	if err := NewRequestLog(s.Pool()).Insert(ctx, pipeline.Record{
		RequestID: "req-after-migrate", At: time.Now(),
		InboundProtocol: "anthropic", UserModel: "kimi-k3",
		Outcome: relayclient.OutcomeNormal,
	}); err != nil {
		t.Fatalf("补列后写不进去：%v", err)
	}
}

// 列表查询也要能扫这一列：recordColumns 与 scanRecord 一旦不同步，
// List 会整个报错而不只是少一个字段。
func TestListScansAttemptsTrail(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	log := NewRequestLog(s.Pool())

	if err := log.Insert(ctx, pipeline.Record{
		RequestID: "req-trail-list", At: time.Now(),
		InboundProtocol: "anthropic", UserModel: "kimi-k3",
		Outcome:       relayclient.OutcomeNormal,
		AttemptsTrail: []pipeline.AttemptRecord{{N: 1, Account: "acc-a"}},
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	out, _, err := log.List(ctx, Filter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("list = %d 条", len(out))
	}
	if len(out[0].AttemptsTrail) != 1 || out[0].AttemptsTrail[0].Account != "acc-a" {
		t.Errorf("List 没扫到轨迹：%+v", out[0].AttemptsTrail)
	}
}
