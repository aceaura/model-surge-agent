package httpapi

import (
	"context"
	"runtime"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/aceaura/model-surge-agent/backend/cache"
	"github.com/aceaura/model-surge-agent/backend/contract/agentv1"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
	"github.com/aceaura/model-surge-agent/backend/store"
)

// Checker 探各依赖的可用性。
//
// 两类依赖对整体状态的影响不同。Redis 挂了只丢观测数据，数据面照常工作，
// 所以报 cache: down 但 status: ok。PG 挂了流水与 outbox 都写不了，
// 数据面仍放行（结果尽力直报），但要报 status: degraded 让运维知道
// 这段时间的账可能对不上。
//
// 探测结果带一个很短的窗口，且窗口内的并发命中合并成一次探测。理由是这个
// 端点免鉴权、被容器探针和反代和监控同时盯着，而每次探测的代价不小：一次 PG
// Ping、一次真实的调度层 HTTP 调用、两个 report_outbox 上的全表 count。
// 最后那一项在队列积压时最贵，而队列积压恰恰是探针被看得最紧的时候——
// 不加窗口的话这里会自我放大。
type Checker struct {
	Store  *store.Store
	Outbox *store.Outbox
	Cache  *cache.Cache
	Relay  *relayclient.Client
	// Window 是探测结果的复用窗口，零值取内置 1s。
	//
	// 不做成配置项：1s 相对于容器探针的十秒级周期不会让运维拿到陈旧结果，
	// 而暴露成环境变量会让人以为它值得调。
	Window time.Duration
	// Now 可注入以便测试驱动窗口。
	Now func() time.Time

	sf singleflight.Group

	mu     sync.Mutex
	snap   Health
	snapAt time.Time
}

// probeTimeout 是合并后那一次探测的预算。
//
// 独立于任何一个命中者的 ctx：探测结果要给窗口内所有命中用，
// 不该因为第一个探针超时断开就让后面的人也拿不到。
const probeTimeout = 5 * time.Second

func (c *Checker) Check(ctx context.Context) Health {
	if h, ok := c.fresh(); ok {
		return h
	}
	v, _, _ := c.sf.Do("health", func() (any, error) {
		// 闭包内二次检查：等在合并上的这一组里，可能刚有另一组探完。
		if h, ok := c.fresh(); ok {
			return h, nil
		}
		probeCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), probeTimeout)
		defer cancel()

		h := c.probe(probeCtx)
		c.mu.Lock()
		c.snap, c.snapAt = h, c.now()
		c.mu.Unlock()
		return h, nil
	})
	return v.(Health)
}

// fresh 取仍在窗口内的快照。
func (c *Checker) fresh() (Health, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snapAt.IsZero() || c.now().Sub(c.snapAt) >= c.window() {
		return Health{}, false
	}
	return c.snap, true
}

func (c *Checker) window() time.Duration {
	if c.Window > 0 {
		return c.Window
	}
	return time.Second
}

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// probe 真的去问每个依赖。
func (c *Checker) probe(ctx context.Context) Health {
	h := Health{Status: "ok", Database: "down", Cache: "down", Relay: "down",
		Goroutines: runtime.NumGoroutine()}

	if c.Store != nil && c.Store.Ping(ctx) == nil {
		h.Database = "ok"
		// 池快照只在 Ping 通时取：连接池已被 Close 过的话 Stat() 的数字
		// 是最后一刻的残留，报出去会让运维以为池还活着。
		st := agentv1.PoolStats(c.Store.Stats())
		h.Pool = &st
	}
	if c.Cache == nil {
		// 未配置与配了但连不上是两回事：前者是部署选择，不该报成故障。
		h.Cache = "disabled"
	} else if c.Cache.Ping(ctx) == nil {
		h.Cache = "ok"
	}
	if c.Relay != nil && c.Relay.Ready(ctx) {
		h.Relay = "ok"
	}
	if c.Outbox != nil {
		if pending, dead, err := c.Outbox.Counts(ctx); err == nil {
			h.OutboxPending, h.OutboxDead = pending, dead
		}
	}

	// relay 不可达时数据面无法选目标，等于完全不可用。
	if h.Database != "ok" || h.Relay != "ok" {
		h.Status = "degraded"
	}
	return h
}
