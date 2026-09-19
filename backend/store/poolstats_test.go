package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// 连接池快照的基本形状。
//
// 整个服务变慢而每条流水都不异常时，慢的那段在拿连接上，而那段不在任何
// 一条请求的计时里——这组数是唯一能看的地方。
func TestStatsReportsPoolShape(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	// 借一条连接出来，让 Acquired 必然非零：不借的话池可能全闲，
	// 断言退化成「零等于零」，Acquired 取错字段也测不出来。
	conn, err := s.Pool().Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	st := s.Stats()
	if st.Max <= 0 {
		t.Errorf("max = %d，池上限必须是正数", st.Max)
	}
	if st.Acquired < 1 {
		t.Errorf("acquired = %d，手上正握着一条连接，至少要 1——"+
			"读成 0 说明取的不是 AcquiredConns", st.Acquired)
	}
	if st.Total < st.Acquired {
		t.Errorf("total(%d) < acquired(%d)：总数必须含已借出的那些",
			st.Total, st.Acquired)
	}
	if st.Total > st.Max {
		t.Errorf("total(%d) > max(%d)：总数不可能超过上限", st.Total, st.Max)
	}
	// Total 与 Max 必须是两个不同来源的数。只借一条连接的池远未满，
	// total == max 说明 Total 读的是 MaxConns——那样饱和度永远显示 100%，
	// 而这个字段存在的唯一意义就是看饱和度。
	if st.Total == st.Max {
		t.Errorf("total 与 max 同为 %d：只借了一条连接的池不该刚好满，"+
			"相等说明 Total 读的是 MaxConns", st.Total)
	}
}

// 释放后 Idle 要涨：Acquired 与 Idle 不能是同一个数的两种叫法。
func TestStatsAcquiredAndIdleAreDistinct(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	conn, err := s.Pool().Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	held := s.Stats()
	conn.Release()
	freed := s.Stats()

	if freed.Idle <= held.Idle {
		t.Errorf("释放后 idle 从 %d 变成 %d，应当增加——"+
			"不变说明 Idle 读的不是 IdleConns", held.Idle, freed.Idle)
	}
	if freed.Acquired >= held.Acquired {
		t.Errorf("释放后 acquired 从 %d 变成 %d，应当减少",
			held.Acquired, freed.Acquired)
	}
}

// AcquireWaiting 是累计值，单调不减。
//
// 它不是此刻的排队长度——pgxpool 没有暴露那个数。把它读成瞬时值会让运维
// 以为「现在有 N 个请求在排队」，而实际是「开池以来一共等过 N 次」。
func TestAcquireWaitingIsMonotonicCumulative(t *testing.T) {
	s := open(t)
	first := s.Stats().AcquireWaiting
	second := s.Stats().AcquireWaiting
	if second < first {
		t.Fatalf("acquire_waiting 从 %d 退到 %d：累计值不该减少", first, second)
	}
}

// 池被占满时 AcquireWaiting 必须真的涨。
//
// 归零这个字段在本机测试里测不出来（探针实测：常态下从没人等过连接，
// 断言退化成 0 == 0）。要钉住它就得真把池占满——而这正是它唯一的用途：
// 整个服务变慢、每条流水都正常时，只有这个数会动。
func TestAcquireWaitingRisesWhenPoolIsExhausted(t *testing.T) {
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn(t))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	// 单连接的池：借走一条就必然有人等。
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	defer pool.Close()
	s := &Store{pool: pool}

	held, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	before := s.Stats().AcquireWaiting

	// 第二次取必然要等。它必须**等到**而不是超时：探针实测超时的那次
	// 不计入 EmptyAcquireCount，只有真等到了才算，所以这里得把连接还回去。
	done := make(chan error, 1)
	go func() {
		c, err := pool.Acquire(ctx)
		if err == nil {
			c.Release()
		}
		done <- err
	}()
	time.Sleep(100 * time.Millisecond) // 让那个 goroutine 先卡在等连接上
	held.Release()
	if err := <-done; err != nil {
		t.Fatalf("等待中的那次取连接最终失败了: %v", err)
	}

	after := s.Stats().AcquireWaiting
	if after <= before {
		t.Errorf("acquire_waiting 从 %d 到 %d 没涨：池确实空过、确实有人等过，"+
			"这个数不动说明它没接到 EmptyAcquireCount——"+
			"而整个服务变慢时它是唯一会动的信号", before, after)
	}
}
