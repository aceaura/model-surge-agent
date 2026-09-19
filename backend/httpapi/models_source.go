package httpapi

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/aceaura/model-surge-agent/backend/cache"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
)

// CachedModels 先读缓存，miss 或缓存不可用时回源到调度层。
//
// 缓存这份清单是因为它每次列模型都要用，而内容几分钟才变一次。
// 但它不是必需的：Redis 挂了就每次直连调度层，只是慢一点。
//
// 回源要合并：TTL 到期那一瞬间所有在列客户端一起 miss，不合并的话调度层
// 收到的是 N 倍尖峰；而调度层若正因此不健康，每个请求都要各自等满自己的
// 超时才失败，尖峰会持续整个超时窗口。合并是进程内的事，不依赖 Redis。
//
// 零值可用：sync.Once 之外不需要初始化。
type CachedModels struct {
	Relay *relayclient.Client
	Cache *cache.Cache
	// NegativeTTL 是回源失败后的静默窗口，零值取内置 2s。
	NegativeTTL time.Duration
	// Now 可注入以便测试驱动负缓存窗口。
	Now func() time.Time

	sf singleflight.Group

	mu       sync.Mutex
	failedAt time.Time
	failErr  error
}

// refetchTimeout 是合并后那一次回源的预算。
//
// 独立于任何一个调用方的 ctx：这一次回源的结果是共享的，不该因为发起它的
// 那个客户端断开就作废掉所有等待者的结果。
const refetchTimeout = 10 * time.Second

func (m *CachedModels) List(ctx context.Context) ([]relayclient.UserModelSummary, error) {
	// 命中走最短的路，不进合并：这是绝大多数请求的路径。
	if models, ok := m.Cache.Models(ctx); ok {
		return models, nil
	}
	if err := m.recentFailure(); err != nil {
		return nil, err
	}

	v, err, _ := m.sf.Do("models", func() (any, error) {
		// 闭包内二次检查：领头者在 Do 之外查过一次，此刻可能另一组合并
		// 刚把结果写进去了。少了这一步，紧邻的两组合并会各回源一次。
		if models, ok := m.Cache.Models(ctx); ok {
			return models, nil
		}
		// WithoutCancel 派生：发起方断开不影响这一次回源，它要服务的是
		// 所有等待者以及后面从缓存里读到它的人。
		fetchCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), refetchTimeout)
		defer cancel()

		models, err := m.Relay.Models(fetchCtx)
		if err != nil {
			m.noteFailure(err)
			return nil, err
		}
		m.Cache.PutModels(fetchCtx, models)
		return models, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]relayclient.UserModelSummary), nil
}

// recentFailure 报告是否仍在上次失败的静默窗口内。
//
// 负缓存的理由：调度层不可用时，不记住这件事就等于每个请求各自去撞一次墙、
// 各自等满超时。窗口短到运维察觉不到，长到能把一次尖峰压成一次调用。
func (m *CachedModels) recentFailure() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failErr == nil {
		return nil
	}
	if m.now().Sub(m.failedAt) >= m.negativeTTL() {
		m.failErr = nil
		return nil
	}
	return m.failErr
}

func (m *CachedModels) noteFailure(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failedAt, m.failErr = m.now(), err
}

func (m *CachedModels) negativeTTL() time.Duration {
	if m.NegativeTTL > 0 {
		return m.NegativeTTL
	}
	return 2 * time.Second
}

func (m *CachedModels) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}
