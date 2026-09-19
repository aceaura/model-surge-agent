package store

import (
	"sync"
	"testing"
	"time"
)

// 并发认领不得互相阻塞。
//
// 租约列本身就能保证不重叠（顺序调用也测到了），所以 SKIP LOCKED 守的不是
// 正确性而是「后到的执行流立刻拿到空批」。少了它，后到者会排在行锁上等前者
// 那条语句的事务提交；等的是毫秒级，但关停期那次 Drain 正是在预算紧张时
// 与 ticker 撞上的，而它每多等一轮就少冲出去一批上报。
func TestConcurrentClaimsDoNotBlockEachOther(t *testing.T) {
	o, ctx := freshOutbox(t)
	seedOutbox(t, o, 40)

	// 在一个显式事务里锁住全部到期行，然后按住不提交。行锁的生命期是事务，
	// 所以这段时间里另一个执行流要么跳过、要么排队等提交。
	//
	// 不能用 pg_sleep 混在同一条语句里拖时间：那样 sleep 可能先被求值，锁到
	// 语句末尾才拿上，争用根本不会发生——这条用例就废了而看不出来。
	tx, err := o.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx,
		"SELECT id FROM report_outbox WHERE next_attempt_at <= now() FOR UPDATE"); err != nil {
		t.Fatalf("lock: %v", err)
	}
	slow := make(chan struct{})
	go func() {
		defer close(slow)
		time.Sleep(600 * time.Millisecond)
		_ = tx.Rollback(ctx)
	}()

	start := time.Now()
	if _, err := o.Due(ctx, time.Now(), 10, time.Minute); err != nil {
		t.Fatalf("Due: %v", err)
	}
	elapsed := time.Since(start)
	<-slow

	// 跳过被锁的行应当立刻返回。排队等的实现会在这里等满 pg_sleep。
	if elapsed > 300*time.Millisecond {
		t.Errorf("认领等了 %s，说明它排在行锁上而不是跳过", elapsed)
	}
}

// 租约长度传零要兜底，不能认领出一个立刻过期的租约。
//
// 零租约等于没有租约：这一行在下一个执行流眼里马上就重新可见，于是
// 两边各做一次裁决，attempts 又开始虚涨——正是这一整轮要修的那个形态。
func TestZeroLeaseFallsBackToDefault(t *testing.T) {
	o, ctx := freshOutbox(t)
	seedOutbox(t, o, 1)

	now := time.Now()
	if _, err := o.Due(ctx, now, 10, 0); err != nil {
		t.Fatalf("Due: %v", err)
	}
	// 同一时刻再认领：租约兜底成非零的话这里应当是空批。
	again, err := o.Due(ctx, now, 10, time.Minute)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("零租约下又认领到 %d 条，租约没兜底", len(again))
	}
}

// 一次裁决落地就要交还租约。
//
// 不交还的话，这一行在租约剩余时间里仍然隐身。退避是 1 秒起步而租约是
// 三十秒起步，于是一条排在一秒后重试的上报实际要等三十秒才会被再看一眼——
// 队列排空速度被租约长度而不是退避曲线支配。
func TestRetryReturnsTheLease(t *testing.T) {
	o, ctx := freshOutbox(t)
	seedOutbox(t, o, 1)

	now := time.Now()
	due, err := o.Due(ctx, now, 10, time.Hour)
	if err != nil || len(due) != 1 {
		t.Fatalf("Due: %d 条, err = %v", len(due), err)
	}
	// 排到一秒后重试，而租约还有一小时。
	next := now.Add(time.Second)
	if err := o.Retry(ctx, due[0].ID, due[0].LeaseToken, next, "boom"); err != nil {
		t.Fatalf("Retry: %v", err)
	}

	again, err := o.Due(ctx, next.Add(time.Millisecond), 10, time.Minute)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(again) != 1 {
		t.Errorf("到了重试时刻却认领到 %d 条——租约没交还，"+
			"排空速度被租约长度而不是退避曲线支配", len(again))
	}
}

// 认领不得把同一行发给两个真并发的执行流。
//
// 与顺序版那条的区别：这条把两次认领放进两个 goroutine，覆盖的是
// 「同一瞬间」那一半机制。
func TestTrulyConcurrentClaimsAreDisjoint(t *testing.T) {
	o, ctx := freshOutbox(t)
	seedOutbox(t, o, 20)

	now := time.Now()
	var (
		wg   sync.WaitGroup
		got  [2][]Entry
		errs [2]error
	)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], errs[i] = o.Due(ctx, now, 20, time.Minute)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 个执行流: %v", i, err)
		}
	}
	seen := map[int64]bool{}
	for _, e := range got[0] {
		seen[e.ID] = true
	}
	for _, e := range got[1] {
		if seen[e.ID] {
			t.Errorf("id %d 被两个真并发的执行流同时认领", e.ID)
		}
	}
	if len(got[0])+len(got[1]) != 20 {
		t.Errorf("两边共认领 %d 条，want 20——有行被漏掉了",
			len(got[0])+len(got[1]))
	}
}
