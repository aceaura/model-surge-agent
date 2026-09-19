package store

import (
	"context"
	"testing"
)

// 设了上限就要生效，且能从 /healthz 的池快照里看到。
//
// 这个池被四路共用：每请求的流水落库、outbox worker、流水清理循环、
// 健康检查。此前代码里搜不到任何一处设置，走的是驱动默认（pgx 是 32），
// 而 PG 侧 max_connections 通常是 100 且可能被三个服务共用。池打满时
// 落库只 Warn、数据面照常 200，症状是「流水随机缺行」——不触发任何告警。
func TestMaxConnsIsApplied(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, dsn(t), Options{MaxConns: 7})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Close)

	if got := s.Stats().Max; got != 7 {
		t.Errorf("pool max = %d, want 7——配的上限没生效", got)
	}
}

// 零值沿用驱动默认，不得把上限设成 0。
//
// 设成 0 的池一条连接都建不出来，而症状是每次取连接都超时——
// 一个比「上限不可配」坏得多的结果。
func TestZeroMaxConnsKeepsDriverDefault(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, dsn(t), Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Close)

	if got := s.Stats().Max; got <= 0 {
		t.Errorf("pool max = %d，零值被当成了上限本身", got)
	}
}
