// Command server 是 model-surge-agent 的入口：装配各层并起 HTTP 服务。
//
// 装配顺序即依赖顺序：config → store → cache → relayclient → pipeline → httpapi。
// codec 靠空导入在 init 期注册，所以 import 块里那几行不是多余的——少一行就少一个协议。
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aceaura/model-surge-agent/backend/cache"
	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/config"
	"github.com/aceaura/model-surge-agent/backend/httpapi"
	"github.com/aceaura/model-surge-agent/backend/outbox"
	"github.com/aceaura/model-surge-agent/backend/pipeline"
	"github.com/aceaura/model-surge-agent/backend/recorder"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
	"github.com/aceaura/model-surge-agent/backend/store"

	// 协议编解码在 init 期注册进 codec 注册表。删掉一行就等于下线一个协议，
	// 而症状是运行期的 invalid_model 而非编译错误，所以别当成未使用的导入。
	_ "github.com/aceaura/model-surge-agent/backend/codec/anthropic"
	_ "github.com/aceaura/model-surge-agent/backend/codec/chatcompletions"
	_ "github.com/aceaura/model-surge-agent/backend/codec/gemini"
	_ "github.com/aceaura/model-surge-agent/backend/codec/responses"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// 用信号 context 而非 Background：收到 SIGTERM 后要让 outbox worker
	// 与在途请求收尾，直接退会把已受理但未上报的结果丢掉。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.PGDSN)
	if err != nil {
		return err
	}
	defer db.Close()

	rdb := cache.New(cache.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
		TTL:      cfg.CacheTTL,
	})
	defer rdb.Close()
	// Redis 不可用不阻止启动：缓存里没有任何东西是必需的。
	if rdb != nil {
		if err := rdb.Ping(ctx); err != nil {
			log.Warn("cache unavailable, running degraded", "error", err)
		}
	}

	relay := relayclient.New(cfg.RelayBaseURL, cfg.RelayDispatchKey)
	queue := store.NewOutbox(db.Pool())
	requests := store.NewRequestLog(db.Pool())

	worker := &outbox.Worker{
		Queue:  queue,
		Sender: relay,
		Log:    log,
		Opts: outbox.Options{
			Interval:    cfg.OutboxInterval,
			MaxAttempts: cfg.OutboxMaxAttempts,
		},
	}
	go worker.Run(ctx)
	go pruneLoop(ctx, log, requests, cfg.LogRetention)

	models := httpapi.CachedModels{Relay: relay, Cache: rdb}
	health := httpapi.Checker{Store: db, Outbox: queue, Cache: rdb, Relay: relay}

	srv := &httpapi.Server{
		Pipeline: &pipeline.Pipeline{
			Dispatch: relay,
			Reporter: worker,
			Recorder: &recorder.Recorder{Log: requests, Cache: rdb, Logger: log},
			Opts: pipeline.Options{
				MaxAttempts:       cfg.MaxAttempts,
				FirstTokenTimeout: cfg.FirstTokenTimeout,
				IdleTimeout:       cfg.IdleTimeout,
				EstimateUsage:     cfg.EstimateUsage,
			},
		},
		Models:      models,
		Health:      health,
		Log:         log,
		AccessLog:   cfg.AccessLog,
		CORSOrigins: cfg.CORSOrigins,
		Admin: &httpapi.Admin{
			Key:      cfg.AdminKey,
			Requests: requests,
			Outbox:   queue,
			Cache:    rdb,
			Models:   models,
			Health:   health,
		},
	}

	listener := &http.Server{
		Addr:    cfg.Listen,
		Handler: srv.Handler(),
		// 不设 WriteTimeout：SSE 响应会持续数分钟，写超时会把流从中间掐断。
		// 空闲与首字超时由 pipeline 按流的语义控制。
		ReadHeaderTimeout: 15 * time.Second,
	}

	log.Info("listening",
		"addr", cfg.Listen,
		"inbound", codec.InboundNames(),
		"outbound", codec.OutboundNames(),
		// 公共数据面无自身鉴权：客户端凭据转给调度层比对。提醒运维别裸暴露。
		"public_plane_auth", "delegated to relay",
	)

	errs := make(chan error, 1)
	go func() {
		if err := listener.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := listener.Shutdown(shutdownCtx); err != nil {
		log.Warn("graceful shutdown incomplete", "error", err)
	}
	// 退出前把队列里到期的上报冲一次：调度层的用量与冷却依赖它们。
	if sent := worker.Drain(shutdownCtx); sent > 0 {
		log.Info("flushed pending reports on shutdown", "count", sent)
	}
	return nil
}

// pruneLoop 定期清理过期流水。留着不清会让表无限增长，
// 而流水的价值随时间快速衰减。
func pruneLoop(ctx context.Context, log *slog.Logger, requests *store.RequestLog, retention time.Duration) {
	if retention <= 0 {
		return
	}
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := requests.Prune(ctx, time.Now().Add(-retention))
			if err != nil {
				log.Warn("prune request log", "error", err)
				continue
			}
			if n > 0 {
				log.Info("pruned request log", "rows", n)
			}
		}
	}
}
