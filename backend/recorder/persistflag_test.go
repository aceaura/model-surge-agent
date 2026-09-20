package recorder

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/cache"
	"github.com/aceaura/model-surge-agent/backend/pipeline"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
	"github.com/aceaura/model-surge-agent/backend/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// openCache 连测试 Redis，DB 11 专供本文件，避免与别处的断言互相干扰。
func openCache(t *testing.T) *cache.Cache {
	t.Helper()
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR not set")
	}
	c := cache.New(cache.Options{Addr: addr, DB: 11, TTL: time.Minute})
	t.Cleanup(c.Close)
	if err := c.Ping(context.Background()); err != nil {
		t.Skipf("redis unreachable: %v", err)
	}
	return c
}

// 采集点必须把五维都填进摘要与桶。少填就是 /admin/live 与 /admin/requests
// 长期对不上，而两个视图都不报错——这是本轮要消除的那类故障。
func TestRecordFillsEveryUsageDimension(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	id := "req-five-dims"
	now := time.Now().UTC()
	r := &Recorder{Cache: c}
	r.Record(pipeline.Record{
		RequestID:       id,
		At:              now,
		InboundProtocol: "anthropic",
		Outcome:         relayclient.OutcomeNormal,
		LatencyMS:       10,
		Usage: relayclient.Usage{
			InputTokens: 1, OutputTokens: 2,
			CacheReadTokens: 3, CacheWriteTokens: 4, ReasoningTokens: 5,
		},
	})

	var got *cache.LiveEntry
	for _, e := range c.Live(ctx, 50) {
		if e.RequestID == id {
			got = &e
			break
		}
	}
	if got == nil {
		t.Fatalf("摘要里没有 %s", id)
	}
	if got.CacheReadTokens != 3 || got.CacheWriteTokens != 4 || got.ReasoningTokens != 5 {
		t.Errorf("摘要三维 = %d/%d/%d，想要 3/4/5",
			got.CacheReadTokens, got.CacheWriteTokens, got.ReasoningTokens)
	}

	// 桶也要填满：趋势图与实时页是两条独立的写入路径，填一处不算。
	buckets := c.Buckets(ctx, now, time.Minute)
	if len(buckets) == 0 {
		t.Fatal("没有分钟桶")
	}
	last := buckets[len(buckets)-1]
	if last.CacheReadTokens < 3 || last.CacheWriteTokens < 4 || last.ReasoningTokens < 5 {
		t.Errorf("桶三维 = %d/%d/%d，至少该有 3/4/5",
			last.CacheReadTokens, last.CacheWriteTokens, last.ReasoningTokens)
	}
}

// LogPersisted 三态各一格。这是「两个视图分叉」唯一的暴露面：
// 没有它，运维只看到 /admin/requests 少了一条而 /admin/live 有，
// 会把差异当成自己看错了。
func TestLogPersistedIsThreeState(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()

	find := func(t *testing.T, id string) *cache.LiveEntry {
		t.Helper()
		for _, e := range c.Live(ctx, 50) {
			if e.RequestID == id {
				return &e
			}
		}
		t.Fatalf("摘要里没有 %s", id)
		return nil
	}

	t.Run("未配明细表则为 nil", func(t *testing.T) {
		id := "req-no-pg"
		(&Recorder{Cache: c}).Record(pipeline.Record{
			RequestID: id, At: time.Now().UTC(), Outcome: relayclient.OutcomeNormal,
		})
		if got := find(t, id).LogPersisted; got != nil {
			t.Errorf("没配 PG 时该是 nil（未知），实际 %v", *got)
		}
	})

	t.Run("落库失败则为 false", func(t *testing.T) {
		// 指向一个没人监听的端口：池是懒连接的，New 不报错，Insert 才失败。
		// 用真的 RequestLog 而不是假实现——失败路径本身就是要测的东西。
		pool, err := pgxpool.New(context.Background(),
			"postgres://nobody:nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
		if err != nil {
			t.Fatalf("pgxpool.New: %v", err)
		}
		t.Cleanup(pool.Close)
		id := "req-pg-down"
		(&Recorder{Cache: c, Log: store.NewRequestLog(pool), Timeout: 2 * time.Second}).
			Record(pipeline.Record{
				RequestID: id, At: time.Now().UTC(), Outcome: relayclient.OutcomeNormal,
			})
		got := find(t, id).LogPersisted
		if got == nil {
			t.Fatal("尝试过落库，标记不该是 nil")
		}
		if *got {
			t.Error("落库失败了，标记却是 true")
		}
	})

	t.Run("落库成功则为 true", func(t *testing.T) {
		dsn := os.Getenv("TEST_PG_DSN")
		if dsn == "" {
			t.Skip("TEST_PG_DSN not set")
		}
		// 走 store.Open 而不是裸 pgxpool：建表是它的职责，
		// 裸池连上的库可能还没有 request_log 表。
		s, err := store.Open(context.Background(), dsn, store.Options{})
		if err != nil {
			t.Skipf("pg unreachable: %v", err)
		}
		t.Cleanup(s.Close)
		log := store.NewRequestLog(s.Pool())
		id := "req-pg-ok"
		(&Recorder{Cache: c, Log: log}).Record(pipeline.Record{
			RequestID: id, At: time.Now().UTC(), Outcome: relayclient.OutcomeNormal,
			InboundProtocol: "anthropic",
		})
		got := find(t, id).LogPersisted
		if got == nil {
			t.Fatal("尝试过落库，标记不该是 nil")
		}
		if !*got {
			t.Error("落库成功了，标记却是 false")
		}
	})
}
