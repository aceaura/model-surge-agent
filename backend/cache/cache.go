// Package cache 是 Redis 之上的一层薄封装：模型清单缓存、实时流水环、分钟计数。
//
// 全部方法在 Redis 不可用时降级而非报错。缓存里没有任何东西是必需的：
// 清单可以直连调度层拿，实时页与趋势图是观测用途，掉了不影响数据面。
// 因此凡是 Redis 出错的地方都当作「没有」，让调用方走回源或跳过。
//
// 凭据与 target 永不入 Redis。
package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/aceaura/model-surge-agent/backend/relayclient"
	"github.com/redis/go-redis/v9"
)

// 键空间前缀。同一个 Redis 实例可能被三个服务共用，不加前缀会撞键。
const (
	keyModels   = "msa:models"
	keyLive     = "msa:live"
	keyStatFmt  = "msa:stat:%s"
	statMinute  = "200601021504"
	liveMaxLen  = 500
	statTTL     = 2 * time.Hour
	callTimeout = 2 * time.Second
)

type Cache struct {
	rdb *redis.Client
	ttl time.Duration
}

type Options struct {
	Addr     string
	Password string
	DB       int
	TTL      time.Duration
}

// New 建缓存。Addr 为空返回 nil：未配 Redis 是合法部署形态，
// 调用方对 nil 接收者的调用一律走降级路径。
func New(opts Options) *Cache {
	if opts.Addr == "" {
		return nil
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = time.Minute
	}
	return &Cache{
		rdb: redis.NewClient(&redis.Options{
			Addr:     opts.Addr,
			Password: opts.Password,
			DB:       opts.DB,
			// 快速失败：这里每一次失败都走降级而非上抛，客户端库内部重试
			// 只会把调用方的延迟预算烧光，换不来任何正确性。
			MaxRetries:   -1,
			DialTimeout:  callTimeout,
			ReadTimeout:  callTimeout,
			WriteTimeout: callTimeout,
		}),
		ttl: ttl,
	}
}

func (c *Cache) Close() {
	if c == nil {
		return
	}
	_ = c.rdb.Close()
}

// Ping 供健康检查判断 cache 是否可用。
func (c *Cache) Ping(ctx context.Context) error {
	if c == nil {
		return fmt.Errorf("cache not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	return c.rdb.Ping(ctx).Err()
}

// Models 读缓存的模型清单。ok 为假表示要回源。
func (c *Cache) Models(ctx context.Context) ([]relayclient.UserModelSummary, bool) {
	if c == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	raw, err := c.rdb.Get(ctx, keyModels).Bytes()
	if err != nil {
		return nil, false
	}
	var out []relayclient.UserModelSummary
	if err := json.Unmarshal(raw, &out); err != nil {
		// 缓存里是坏数据（旧版本写的格式之类）：当作没有，回源覆盖掉它。
		return nil, false
	}
	return out, true
}

// PutModels 写清单缓存。写失败静默：下次照样回源，不影响正确性。
func (c *Cache) PutModels(ctx context.Context, models []relayclient.UserModelSummary) {
	if c == nil {
		return
	}
	raw, err := json.Marshal(models)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	_ = c.rdb.Set(ctx, keyModels, raw, c.ttl).Err()
}

// LiveEntry 是实时流水的一条摘要。字段是前端实时页要展示的那些，
// 不含凭据也不含对话内容。
type LiveEntry struct {
	RequestID        string    `json:"request_id"`
	At               time.Time `json:"at"`
	InboundProtocol  string    `json:"inbound_protocol"`
	OutboundProtocol string    `json:"outbound_protocol,omitempty"`
	UserModel        string    `json:"user_model"`
	ModelID          string    `json:"model_id,omitempty"`
	Account          string    `json:"account,omitempty"`
	Outcome          string    `json:"outcome"`
	StatusCode       int       `json:"status_code,omitempty"`
	Attempts         int       `json:"attempts,omitempty"`
	Stream           bool      `json:"stream,omitempty"`
	LatencyMS        int       `json:"latency_ms,omitempty"`
	FirstTokenMS     int       `json:"first_token_ms,omitempty"`
	DispatchMS       int       `json:"dispatch_ms,omitempty"`
	UpstreamMS       int       `json:"upstream_ms,omitempty"`
	InputTokens      int64     `json:"input_tokens,omitempty"`
	OutputTokens     int64     `json:"output_tokens,omitempty"`
	// 后三维与明细表（store.RequestLog）的列一一对应。少报它们正是少报
	// 计费权重最偏的那几维（缓存写通常 1.25×、缓存读 0.1×、推理计入输出），
	// 于是 /admin/requests 的明细与本摘要长期对不上，而两侧都不报错。
	CacheReadTokens  int64  `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64  `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  int64  `json:"reasoning_tokens,omitempty"`
	ErrorCode        string `json:"error_code,omitempty"`
	// LogPersisted 是三态：nil 表示没配 PG（未尝试落库），false 表示尝试过
	// 且失败——这条记录不在 /admin/requests 里，true 表示成功。
	//
	// 必须三态：布尔的零值会让「没配 PG」（正常的单进程测试形态）与
	// 「落库失败」（故障）变成同一个值，而看面板的人分不出来。
	LogPersisted *bool `json:"log_persisted,omitempty"`
}

// PushLive 把一条摘要推进环形列表并裁到上限。
//
// 用 LPUSH + LTRIM 而非有序集合：这是个只按时间读最近 N 条的场景，
// 列表的裁剪是 O(1) 常数条，不需要排序结构。
func (c *Cache) PushLive(ctx context.Context, entry LiveEntry) {
	if c == nil {
		return
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	pipe := c.rdb.Pipeline()
	pipe.LPush(ctx, keyLive, raw)
	pipe.LTrim(ctx, keyLive, 0, liveMaxLen-1)
	_, _ = pipe.Exec(ctx)
}

// Live 读最近的摘要，最新在前。Redis 不可用时返回空而非错误：
// 实时页空着比整页报错好，其余面板还能用。
func (c *Cache) Live(ctx context.Context, limit int) []LiveEntry {
	if c == nil {
		return nil
	}
	if limit <= 0 || limit > liveMaxLen {
		limit = 100
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	raws, err := c.rdb.LRange(ctx, keyLive, 0, int64(limit-1)).Result()
	if err != nil {
		return nil
	}
	out := make([]LiveEntry, 0, len(raws))
	for _, raw := range raws {
		var e LiveEntry
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out
}

// Bucket 是一分钟的聚合计数。
type Bucket struct {
	Minute       time.Time        `json:"minute"`
	Total        int64            `json:"total"`
	Outcomes     map[string]int64 `json:"outcomes,omitempty"`
	InputTokens  int64            `json:"input_tokens"`
	OutputTokens int64            `json:"output_tokens"`
	// 与 LiveEntry 同理：这三维不补齐，趋势图的用量就系统性少报。
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
	LatencySumMS     int64 `json:"latency_sum_ms"`
}

// 分钟桶里的固定字段名。outcome 的计数键加前缀区分，
// 否则一个叫 total 的 outcome 会覆盖总数。
const (
	fieldTotal      = "total"
	fieldInput      = "input"
	fieldOutput     = "output"
	fieldCacheRead  = "cache_read"
	fieldCacheWrite = "cache_write"
	fieldReasoning  = "reasoning"
	fieldLatency    = "latency"
	outcomeAffix    = "o:"
)

// Incr 累计一分钟桶。
//
// usage 整个传进来而不是把五维排成五个 int64 参数：那样一行里会有六个同类型
// 标量，调错顺序编译器不报，而记错的是计费维度。传结构体让字段名对位。
func (c *Cache) Incr(ctx context.Context, at time.Time, outcome string,
	usage relayclient.Usage, latencyMS int) {
	if c == nil {
		return
	}
	key := fmt.Sprintf(keyStatFmt, at.UTC().Format(statMinute))
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	pipe := c.rdb.Pipeline()
	pipe.HIncrBy(ctx, key, fieldTotal, 1)
	if outcome != "" {
		pipe.HIncrBy(ctx, key, outcomeAffix+outcome, 1)
	}
	pipe.HIncrBy(ctx, key, fieldInput, usage.InputTokens)
	pipe.HIncrBy(ctx, key, fieldOutput, usage.OutputTokens)
	// 三维各自独立累计，不加权：按单价折算需要 upstream 的定价模型，
	// 把不同单价的维度加进同一个数等于用错权重记账。
	pipe.HIncrBy(ctx, key, fieldCacheRead, usage.CacheReadTokens)
	pipe.HIncrBy(ctx, key, fieldCacheWrite, usage.CacheWriteTokens)
	pipe.HIncrBy(ctx, key, fieldReasoning, usage.ReasoningTokens)
	pipe.HIncrBy(ctx, key, fieldLatency, int64(latencyMS))
	// TTL 每次刷新：桶写完就不再动，靠过期自行清理，不需要额外的清扫任务。
	pipe.Expire(ctx, key, statTTL)
	_, _ = pipe.Exec(ctx)
}

// Buckets 读最近 window 时长内的分钟桶，按时间正序。缺的分钟不补零，
// 由前端决定怎么画空隙。
func (c *Cache) Buckets(ctx context.Context, now time.Time, window time.Duration) []Bucket {
	if c == nil {
		return nil
	}
	minutes := int(window / time.Minute)
	if minutes <= 0 {
		minutes = 60
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	base := now.UTC().Truncate(time.Minute)
	stamps := make([]time.Time, 0, minutes)
	pipe := c.rdb.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, 0, minutes)
	for i := minutes - 1; i >= 0; i-- {
		at := base.Add(-time.Duration(i) * time.Minute)
		stamps = append(stamps, at)
		cmds = append(cmds, pipe.HGetAll(ctx, fmt.Sprintf(keyStatFmt, at.Format(statMinute))))
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil
	}

	out := make([]Bucket, 0, minutes)
	for i, cmd := range cmds {
		fields, err := cmd.Result()
		if err != nil || len(fields) == 0 {
			continue
		}
		out = append(out, bucketFrom(stamps[i], fields))
	}
	return out
}

func bucketFrom(minute time.Time, fields map[string]string) Bucket {
	b := Bucket{Minute: minute, Outcomes: map[string]int64{}}
	for k, v := range fields {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			continue
		}
		switch k {
		case fieldTotal:
			b.Total = n
		case fieldInput:
			b.InputTokens = n
		case fieldOutput:
			b.OutputTokens = n
		case fieldCacheRead:
			b.CacheReadTokens = n
		case fieldCacheWrite:
			b.CacheWriteTokens = n
		case fieldReasoning:
			b.ReasoningTokens = n
		case fieldLatency:
			b.LatencySumMS = n
		default:
			if len(k) > len(outcomeAffix) && k[:len(outcomeAffix)] == outcomeAffix {
				b.Outcomes[k[len(outcomeAffix):]] = n
			}
		}
	}
	return b
}
