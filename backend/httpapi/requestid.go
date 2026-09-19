package httpapi

import (
	"container/list"
	"sync"
)

// idGuardCap 是近期 id 环的容量。
//
// 必须有界：无界集合等于给任何客户端一个用请求 id 撑爆内存的口子。
// 4096 在本服务的量级下覆盖数秒到数十秒的在途请求，而撞号的危害
// （覆盖流水、丢上报）只在同期请求之间才有意义。
const idGuardCap = 4096

// idGuard 记住近期采纳过的请求 id，供撞号检测。
//
// 存在的理由是撞号的后果全是静默的：request_log 的主键写入是
// ON CONFLICT DO UPDATE（第二个请求覆盖第一个的整条流水）、上报的
// report_id 是 ON CONFLICT DO NOTHING（第二条被当重放丢弃，调度层少记
// 这次的 token）、捕获快照同样被覆盖。既有的 validRequestID 只校验字符集
// 与长度，与撞号无关。
//
// 只在进程内记：多实例部署下两个实例各有自己的环，跨实例撞号仍可能覆盖。
// 修它要在每次请求上加一次共享存储往返，代价与收益不成比例。这条边界
// 写在 docs/api.md 里，不靠代码假装已解决。
type idGuard struct {
	mu    sync.Mutex
	seen  map[string]*list.Element
	order *list.List
}

func newIDGuard() *idGuard {
	return &idGuard{seen: map[string]*list.Element{}, order: list.New()}
}

// claim 登记一个 id，返回该用的记录键与是否撞号。
//
// 未撞号时原样返回入参，键的形态与这套机制不存在时完全一致——不能因为
// 加了这道检查就让所有既有记录换形态。
func (g *idGuard) claim(id string) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if _, ok := g.seen[id]; ok {
		// 撞号：换一个本服务生成的键。不复用客户端那个值做前缀——
		// 前缀化的键仍会撞（同一个客户端 id 撞两次就生成两个同形状的键，
		// 而它们之间没有任何区分），而 randomID 每次都不同。
		return g.remember(randomID()), true
	}
	return g.remember(id), false
}

// remember 把 id 记进环并淘汰最老的，返回入参以便调用方直接用。
func (g *idGuard) remember(id string) string {
	g.seen[id] = g.order.PushBack(id)
	for g.order.Len() > idGuardCap {
		front := g.order.Front()
		g.order.Remove(front)
		delete(g.seen, front.Value.(string))
	}
	return id
}
