// Package recorder 把一条请求流水分发到三处：PG 的持久流水、Redis 的实时环、
// Redis 的分钟计数。
//
// 三处都是尽力而为：数据面已经把响应交给客户端了，观测数据写不进去不该
// 影响那次调用的结果。所以这里不返回错误，失败只打日志。
package recorder

import (
	"context"
	"log/slog"
	"time"

	"github.com/aceaura/model-surge-agent/backend/cache"
	"github.com/aceaura/model-surge-agent/backend/pipeline"
	"github.com/aceaura/model-surge-agent/backend/store"
)

type Recorder struct {
	Log    *store.RequestLog
	Cache  *cache.Cache
	Logger *slog.Logger
	// Timeout 限制单条写入的耗时，0 用默认值。
	Timeout time.Duration
}

const defaultTimeout = 3 * time.Second

// Record 落一条流水。
//
// 用 context.Background 而非请求的 ctx：客户端断开时请求 ctx 立刻取消，
// 但这条流水正是要记录「客户端断开了」这件事，跟着一起取消就什么都留不下。
func (r *Recorder) Record(rec pipeline.Record) {
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout())
	defer cancel()

	// 落库结果要带进摘要：PG 失败只 Warn 而 Redis 两路照常成功，于是
	// /admin/requests 缺记录、/admin/live 有记录，长期不一致且没有任何
	// 暴露面——看面板的人会把差异当成自己看错了。
	var persisted *bool
	if r.Log != nil {
		ok := true
		if err := r.Log.Insert(ctx, rec); err != nil {
			// PG 挂了不拦数据面，只是流水缺一条。
			r.logger().Warn("record request log", "request_id", rec.RequestID, "err", err)
			ok = false
		}
		persisted = &ok
	}

	r.Cache.PushLive(ctx, cache.LiveEntry{
		RequestID:          rec.RequestID,
		At:                 rec.At,
		InboundProtocol:    rec.InboundProtocol,
		OutboundProtocol:   rec.OutboundProtocol,
		UserModel:          rec.UserModel,
		ModelID:            rec.ModelID,
		Account:            rec.Account,
		Outcome:            rec.Outcome,
		StatusCode:         rec.StatusCode,
		Attempts:           rec.Attempts,
		Stream:             rec.Stream,
		LatencyMS:          rec.LatencyMS,
		FirstTokenMS:       rec.FirstTokenMS,
		DispatchMS:         rec.DispatchMS,
		UpstreamMS:         rec.UpstreamMS,
		InputTokens:        rec.Usage.InputTokens,
		OutputTokens:       rec.Usage.OutputTokens,
		CacheReadTokens:    rec.Usage.CacheReadTokens,
		CacheWriteTokens:   rec.Usage.CacheWriteTokens,
		ReasoningTokens:    rec.Usage.ReasoningTokens,
		CacheWrite5mTokens: rec.Usage.CacheWrite5mTokens,
		CacheWrite1hTokens: rec.Usage.CacheWrite1hTokens,
		ErrorCode:          rec.ErrorCode,
		LogPersisted:       persisted,
	})
	r.Cache.Incr(ctx, rec.At, rec.Outcome, rec.Usage, rec.LatencyMS)
}

func (r *Recorder) timeout() time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}
	return defaultTimeout
}

func (r *Recorder) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}
