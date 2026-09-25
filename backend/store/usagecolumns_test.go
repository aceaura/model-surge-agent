package store

import (
	"context"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/pipeline"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
)

// usageAllThirteen 是十三位各不相同的一组用量。
//
// 十三个值刻意互不相同且都非零：同为 BIGINT 的列在列序与 Scan 序错位后照样扫得成功，
// 只是值互换；给相同的值或留零值就测不出错位，而错位的症状是运维看到的成本
// 归因整个错位。5m/1h 两位还要与合计位（4004）不同：明细恰好等于合计时
// 「搬错到合计列」也测不出来。
func usageAllThirteen() relayclient.Usage {
	return relayclient.Usage{
		InputTokens:              1001,
		OutputTokens:             2002,
		CacheReadTokens:          3003,
		CacheWriteTokens:         4004,
		ReasoningTokens:          5005,
		CacheWrite5mTokens:       6006,
		CacheWrite1hTokens:       7007,
		WebSearchRequests:        8008,
		WebFetchRequests:         9009,
		PromptAudioTokens:        10010,
		CompletionAudioTokens:    11011,
		AcceptedPredictionTokens: 12012,
		RejectedPredictionTokens: 13013,
	}
}

func assertUsage(t *testing.T, got relayclient.Usage) {
	t.Helper()
	want := usageAllThirteen()
	if got.CacheWriteTokens != want.CacheWriteTokens {
		t.Errorf("cache_write_tokens = %d，want %d；记零意味着缓存写入的成本"+
			"完全不入账，而客户端那侧确实收到了这个数字",
			got.CacheWriteTokens, want.CacheWriteTokens)
	}
	if got.ReasoningTokens != want.ReasoningTokens {
		t.Errorf("reasoning_tokens = %d，want %d", got.ReasoningTokens, want.ReasoningTokens)
	}
	if got != want {
		t.Errorf("用量整体 = %+v，want %+v（值互换说明列序与 Scan 序错位）", got, want)
	}
}

// 判据 3：十三位用量的落库往返。
func TestUsageThirteenDimensionsRoundTrip(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	log := NewRequestLog(s.Pool())

	in := pipeline.Record{
		RequestID:       "usage-5",
		InboundProtocol: "anthropic",
		UserModel:       "kimi-k3",
		Outcome:         "normal",
		Usage:           usageAllThirteen(),
	}
	if err := log.Insert(ctx, in); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := log.Get(ctx, "usage-5")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	assertUsage(t, got.Usage)
}

// 判据 4：撞号重写（ON CONFLICT DO UPDATE）也要更新这两位。
//
// 单独一条而不是并进上面：DO UPDATE 的列清单与 INSERT 的列清单是两处，
// 漏掉其中一处时首次插入正确、重写后那两位粘住旧值。一次请求的终态上报
// 走的正是重写这条路（受理面与终态可能两次写同一个 request_id）。
func TestUsageThirteenDimensionsSurviveUpsert(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	log := NewRequestLog(s.Pool())

	base := pipeline.Record{
		RequestID:       "usage-upsert",
		InboundProtocol: "anthropic",
		UserModel:       "kimi-k3",
		Outcome:         "retrying",
	}
	// 第一次写：两位都是零（还没收到用量）。
	if err := log.Insert(ctx, base); err != nil {
		t.Fatalf("首次 insert: %v", err)
	}
	// 终态重写：用量到了。
	base.Outcome = "normal"
	base.Usage = usageAllThirteen()
	if err := log.Insert(ctx, base); err != nil {
		t.Fatalf("重写 insert: %v", err)
	}
	got, err := log.Get(ctx, "usage-upsert")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	assertUsage(t, got.Usage)
}

// 判据 5：老库的补列路径。
//
// 测试库是新建的，CREATE TABLE 已经建齐两列，所以把 schema.sql 尾部那两条
// ALTER 删掉整套测试照样通过（R19 同型教训）。要主动 DROP COLUMN 再重跑
// 迁移，才测到「先于这两列建起的旧表升级上来」这条路。
func TestUsageColumnsAreAddedToOlderTables(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	for _, col := range []string{"cache_write_tokens", "reasoning_tokens",
		"cache_write_5m_tokens", "cache_write_1h_tokens",
		"web_search_requests", "web_fetch_requests",
		"prompt_audio_tokens", "completion_audio_tokens",
		"accepted_prediction_tokens", "rejected_prediction_tokens"} {
		if _, err := s.Pool().Exec(ctx,
			"ALTER TABLE request_log DROP COLUMN IF EXISTS "+col); err != nil {
			t.Fatalf("drop %s: %v", col, err)
		}
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("重跑迁移：%v", err)
	}

	log := NewRequestLog(s.Pool())
	in := pipeline.Record{
		RequestID:       "usage-migrated",
		InboundProtocol: "anthropic",
		UserModel:       "kimi-k3",
		Outcome:         "normal",
		Usage:           usageAllThirteen(),
	}
	if err := log.Insert(ctx, in); err != nil {
		t.Fatalf("补列后 insert: %v", err)
	}
	got, err := log.Get(ctx, "usage-migrated")
	if err != nil {
		t.Fatalf("补列后 get: %v", err)
	}
	assertUsage(t, got.Usage)
}
