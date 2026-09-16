package httpapi

import (
	"context"

	"github.com/aceaura/model-surge-agent/backend/cache"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
)

// CachedModels 先读缓存，miss 或缓存不可用时回源到调度层。
//
// 缓存这份清单是因为它每次列模型都要用，而内容几分钟才变一次。
// 但它不是必需的：Redis 挂了就每次直连调度层，只是慢一点。
type CachedModels struct {
	Relay *relayclient.Client
	Cache *cache.Cache
}

func (m CachedModels) List(ctx context.Context) ([]relayclient.UserModelSummary, error) {
	if models, ok := m.Cache.Models(ctx); ok {
		return models, nil
	}
	models, err := m.Relay.Models(ctx)
	if err != nil {
		return nil, err
	}
	m.Cache.PutModels(ctx, models)
	return models, nil
}
