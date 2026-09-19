package httpapi

import (
	"context"
	"runtime"

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
type Checker struct {
	Store  *store.Store
	Outbox *store.Outbox
	Cache  *cache.Cache
	Relay  *relayclient.Client
}

func (c Checker) Check(ctx context.Context) Health {
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
