// Package store 持有 PostgreSQL 连接池并在启动时建表。
package store

import (
	"context"
	"embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaFS embed.FS

type Store struct {
	pool *pgxpool.Pool
}

// Options 是连接池的可调参数。
type Options struct {
	// MaxConns 是池上限。零值沿用驱动默认（pgx 是 32）。
	//
	// 需要可配是因为这个池被四路共用：每请求的流水落库、outbox worker、
	// 流水清理循环、健康检查。PG 侧 max_connections 通常是 100，而本服务、
	// 调度层、配置中心可能共用一台。池被打满时落库只 Warn、数据面照常 200，
	// 症状是「流水随机缺行」——一个不会触发任何告警的症状。
	//
	// 默认不改成某个具体值：在这里单方面抬高上限只会把耗尽点从本服务
	// 挪到共用同一台 PG 的别人身上。可配加上 /healthz 里的 pool.max 与
	// acquire_waiting，运维就有据可调。
	MaxConns int
}

func Open(ctx context.Context, dsn string, opts Options) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse postgres dsn: %w", err)
	}
	if opts.MaxConns > 0 {
		cfg.MaxConns = int32(opts.MaxConns)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	s := &Store{pool: pool}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate(ctx context.Context) error {
	ddl, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("read schema: %w", err)
	}
	if _, err := s.pool.Exec(ctx, string(ddl)); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// PoolStats 是连接池的饱和度快照。
//
// 自定义结构体而不是把 *pgxpool.Stat 漏给 httpapi：那个类型有二十来个方法，
// 直接暴露会让健康检查的响应形状随驱动升级而漂移。
type PoolStats struct {
	// Acquired 是此刻被借出的连接数，Idle 是池里闲着的，Total 是两者之和
	// 加上正在建立的。Max 是上限。
	Acquired int32 `json:"acquired"`
	Idle     int32 `json:"idle"`
	Total    int32 `json:"total"`
	Max      int32 `json:"max"`
	// AcquireWaiting 是**累计**发生过「池空、只能等」的次数，不是此刻的
	// 排队长度——pgxpool 没有暴露瞬时排队数。累计值反而更好用：它单调递增，
	// 运维两次取样做差就知道这段时间有没有人等过连接，而瞬时值在轮询间隙里
	// 等过又等到了会完全看不见。
	AcquireWaiting int64 `json:"acquire_waiting"`
}

// Stats 现取连接池快照。Stat() 是读内存计数器，没有 IO，
// 所以不需要后台采样 goroutine——那只会换来一个过时的数字，
// 而运维调 /health 就是想知道此刻的饱和度。
func (s *Store) Stats() PoolStats {
	st := s.pool.Stat()
	return PoolStats{
		Acquired:       st.AcquiredConns(),
		Idle:           st.IdleConns(),
		Total:          st.TotalConns(),
		Max:            st.MaxConns(),
		AcquireWaiting: st.EmptyAcquireCount(),
	}
}

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func (s *Store) Close() { s.pool.Close() }
