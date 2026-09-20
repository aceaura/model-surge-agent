// Package config 从环境变量装载配置。没有配置文件：容器化部署下
// 环境变量是唯一来源，两套机制并存只会让「当前生效值是哪个」变得难以回答。
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aceaura/model-surge-agent/backend/capture"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
)

type Config struct {
	Listen string

	PGDSN string
	// PGMaxConns 是连接池上限。0 表示沿用驱动默认。
	PGMaxConns    int
	RedisAddr     string
	RedisPassword string
	RedisDB       int
	CacheTTL      time.Duration

	RelayBaseURL     string
	RelayDispatchKey string
	AdminKey         string

	MaxAttempts       int
	FirstTokenTimeout time.Duration
	IdleTimeout       time.Duration
	HeartbeatInterval time.Duration
	// MaxRequestDuration 是一次请求的总时长上限（含全部重试）。0 表示不限。
	MaxRequestDuration time.Duration

	OutboxInterval    time.Duration
	OutboxMaxAttempts int

	// 关停三阶段的预算。每阶段一份，且各自新建 context：
	// 让后一阶段继承前一阶段用尽的 deadline 会使收尾变成空操作。
	//
	// ShutdownGrace 未显式设置时派生自流超时（取首字与空闲的较大者），
	// 不写死常量：写死会随流超时被调大而重新变得短于它，
	// 于是「默认配置自相矛盾」这个状态又回来了。
	ShutdownGrace        time.Duration
	ShutdownLinger       time.Duration
	ShutdownFlushTimeout time.Duration

	EstimateUsage bool
	AccessLog     bool
	LogRetention  time.Duration

	// CORSOrigins 是允许跨域的来源。空或含 "*" 表示放开所有来源，
	// 此时不发 Allow-Credentials——浏览器拒绝 `*` 与凭据并存。
	CORSOrigins []string

	// 出站连接层。零值一律取 pipeline 的内置默认。
	MaxIdleConns        int
	MaxIdleConnsPerHost int
	IdleConnTimeout     time.Duration
	// ResponseHeaderTimeout 只约束「发出请求到响应头到达」这一段，
	// 头到了之后读正文不受它影响。负值表示显式不设限。
	ResponseHeaderTimeout time.Duration
	// H2 死连接探测的两个超时。任一为负表示显式关闭探测。
	H2SendPingTimeout time.Duration
	H2PingTimeout     time.Duration

	// 控制面（调 relay 的调度面）连接层。零值一律取 relayclient 的内置默认。
	//
	// 与出站那一组分开：控制面是内网的小 JSON 往返，出站要容忍推理模型在首帧前
	// 思考几分钟。共用一组值等于用数据面的宽松度去等一个本该秒回的内部查询。
	RelayMaxIdleConns          int
	RelayMaxIdleConnsPerHost   int
	RelayIdleConnTimeout       time.Duration
	RelayResponseHeaderTimeout time.Duration
	RelayTimeout               time.Duration
	// RelayProxy 是 off/environment 两态。默认 off：控制面的 baseURL 在集群里
	// 是服务名，而这类主机名会命中 HTTP_PROXY，于是调度调用被送去一个不认识它
	// 的外网代理，症状却是「relay 不可达」。
	RelayProxy string

	// 转换四体捕获。CaptureMode 为 off/errors/all 三态，非法值在启动时报错
	// 而不是静默关闭：静默的后果是运维以为捕获开着，等出了故障才发现
	// 什么都没留，而那时故障已经过去了。
	CaptureMode       string
	CaptureMaxBody    int
	CaptureMaxEntries int
}

// Load 收集所有问题一次报全，而不是逐个失败：改配置的人通常在容器日志里
// 只看得到第一条错误，来回重启几轮才配对是很差的体验。
func Load() (Config, error) {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	c := Config{
		Listen:           envOr("MSA_LISTEN", ":8080"),
		PGDSN:            os.Getenv("MSA_PG_DSN"),
		RedisAddr:        os.Getenv("MSA_REDIS_ADDR"),
		RedisPassword:    os.Getenv("MSA_REDIS_PASSWORD"),
		RelayBaseURL:     strings.TrimRight(os.Getenv("MSA_RELAY_BASE_URL"), "/"),
		RelayDispatchKey: os.Getenv("MSA_RELAY_DISPATCH_KEY"),
		AdminKey:         os.Getenv("MSA_ADMIN_KEY"),
	}

	if c.PGDSN == "" {
		fail("MSA_PG_DSN is required")
	}
	if c.RelayBaseURL == "" {
		fail("MSA_RELAY_BASE_URL is required")
	}
	if c.RelayDispatchKey == "" {
		fail("MSA_RELAY_DISPATCH_KEY is required")
	}
	if c.AdminKey == "" {
		fail("MSA_ADMIN_KEY is required")
	}
	// 两把密钥职责不同：一把对调度层出示，一把守管理面。
	// 相同则任何能读管理面的人都能冒充本服务发调度请求。
	if c.AdminKey != "" && c.AdminKey == c.RelayDispatchKey {
		fail("MSA_ADMIN_KEY must differ from MSA_RELAY_DISPATCH_KEY")
	}

	// 0 表示沿用驱动默认，负值是配错了。不把负值当成「不限制」：
	// 连接池没有「不限制」这个语义，写负数的人多半是想表达那个不存在的意思。
	c.PGMaxConns = intOr(&errs, "MSA_PG_MAX_CONNS", 0)
	if c.PGMaxConns < 0 {
		fail("MSA_PG_MAX_CONNS must be positive, or 0 to use the driver default")
	}

	c.RedisDB = intOr(&errs, "MSA_REDIS_DB", 0)
	c.CacheTTL = durationOr(&errs, "MSA_CACHE_TTL", time.Minute)
	c.MaxAttempts = intOr(&errs, "MSA_MAX_ATTEMPTS", 3)
	if c.MaxAttempts < 1 {
		fail("MSA_MAX_ATTEMPTS must be at least 1")
	}
	c.FirstTokenTimeout = durationOr(&errs, "MSA_FIRST_TOKEN_TIMEOUT", 60*time.Second)
	c.IdleTimeout = durationOr(&errs, "MSA_IDLE_TIMEOUT", 120*time.Second)
	// 负值表示显式关闭保活，所以这里不做 > 0 校验。
	c.HeartbeatInterval = durationOr(&errs, "MSA_HEARTBEAT_INTERVAL", 15*time.Second)
	// 心跳只在流式读循环里发，比两个流超时还长的间隔意味着一个心跳都发不出——
	// 请求先被超时掐断。那是「配了保活却等于没配」，而运维完全看不出来，
	// 所以拒绝启动而不是打个警告。
	//
	// 取两者较小的那个：空闲超时比心跳还短时，静默期里同样一个都发不出。
	// 用 >= 而不是 >：恰好相等时谁先到取决于调度，这种配置没有存在的理由。
	if c.HeartbeatInterval > 0 {
		minTimeout := c.FirstTokenTimeout
		if c.IdleTimeout < minTimeout {
			minTimeout = c.IdleTimeout
		}
		if c.HeartbeatInterval >= minTimeout {
			fail("MSA_HEARTBEAT_INTERVAL (%s) must be shorter than both "+
				"MSA_FIRST_TOKEN_TIMEOUT (%s) and MSA_IDLE_TIMEOUT (%s); "+
				"otherwise no heartbeat is ever sent and the keepalive is silently off",
				c.HeartbeatInterval, c.FirstTokenTimeout, c.IdleTimeout)
		}
	}
	c.MaxRequestDuration = durationOr(&errs, "MSA_MAX_REQUEST_DURATION", 0)
	if c.MaxRequestDuration < 0 {
		fail("MSA_MAX_REQUEST_DURATION must not be negative; use 0 to disable the limit")
	}
	// 总上限至少要放得下一次完整的尝试：比单次尝试的超时还小的话，
	// 每个请求都会在同一时刻被总上限掐断，重试与单次超时全部失效，
	// 而失效方式是「所有请求都在 N 秒时失败」这种看上去像上游故障的形状。
	if c.MaxRequestDuration > 0 {
		minTotal := c.FirstTokenTimeout
		if c.IdleTimeout > minTotal {
			minTotal = c.IdleTimeout
		}
		if c.MaxRequestDuration < minTotal {
			fail("MSA_MAX_REQUEST_DURATION (%s) must be at least "+
				"max(MSA_FIRST_TOKEN_TIMEOUT, MSA_IDLE_TIMEOUT) (%s)",
				c.MaxRequestDuration, minTotal)
		}
	}
	c.OutboxInterval = durationOr(&errs, "MSA_OUTBOX_INTERVAL", time.Second)
	c.OutboxMaxAttempts = intOr(&errs, "MSA_OUTBOX_MAX_ATTEMPTS", 20)

	// 关停宽限期的下限是流超时本身：比它小就意味着一条正当地处于静默
	// 思考期的流每次部署都会被掐断，而客户端看到的是连接中断而非错误。
	minGrace := c.FirstTokenTimeout
	if c.IdleTimeout > minGrace {
		minGrace = c.IdleTimeout
	}
	// 判「是否显式设置」看环境变量在不在，而不是看值是否为零：
	// 显式写 0s 的人意图是「不等」，那应当被下面那条规则拒掉并给出理由，
	// 而不是被当成未设置从而静默取一个很大的默认。
	if os.Getenv("MSA_SHUTDOWN_GRACE") == "" {
		c.ShutdownGrace = minGrace
	} else {
		c.ShutdownGrace = durationOr(&errs, "MSA_SHUTDOWN_GRACE", minGrace)
		if c.ShutdownGrace < minGrace {
			fail("MSA_SHUTDOWN_GRACE (%s) must be at least "+
				"max(MSA_FIRST_TOKEN_TIMEOUT, MSA_IDLE_TIMEOUT) (%s): "+
				"a smaller grace truncates streams that are legitimately still silent",
				c.ShutdownGrace, minGrace)
		}
	}
	// 这两个不接受非正值表达「关闭」：连接层那几个参数用负值关闭是因为
	// 关掉它们是合理的运维选择，而「不等在途、不冲队列」不是——
	// 那恰好就是本轮要修掉的那两个缺陷的样子。
	c.ShutdownLinger = durationOr(&errs, "MSA_SHUTDOWN_LINGER", 30*time.Second)
	if c.ShutdownLinger <= 0 {
		fail("MSA_SHUTDOWN_LINGER must be positive")
	}
	c.ShutdownFlushTimeout = durationOr(&errs, "MSA_SHUTDOWN_FLUSH_TIMEOUT", 10*time.Second)
	if c.ShutdownFlushTimeout <= 0 {
		fail("MSA_SHUTDOWN_FLUSH_TIMEOUT must be positive")
	}
	c.EstimateUsage = boolOr(&errs, "MSA_ESTIMATE_USAGE", true)
	c.AccessLog = boolOr(&errs, "MSA_ACCESS_LOG", true)
	c.LogRetention = durationOr(&errs, "MSA_LOG_RETENTION", 14*24*time.Hour)
	c.CORSOrigins = listOr("MSA_CORS_ORIGINS")

	// 零值交给 pipeline 取内置默认，不在这里重复一份默认值：
	// 两处各写一份迟早会漂移，而漂移后看配置看不出实际生效的是哪个。
	c.MaxIdleConns = intOr(&errs, "MSA_MAX_IDLE_CONNS", 0)
	c.MaxIdleConnsPerHost = intOr(&errs, "MSA_MAX_IDLE_CONNS_PER_HOST", 0)
	c.IdleConnTimeout = durationOr(&errs, "MSA_IDLE_CONN_TIMEOUT", 0)
	c.ResponseHeaderTimeout = durationOr(&errs, "MSA_RESPONSE_HEADER_TIMEOUT", 0)
	c.H2SendPingTimeout = durationOr(&errs, "MSA_H2_SEND_PING_TIMEOUT", 0)
	c.H2PingTimeout = durationOr(&errs, "MSA_H2_PING_TIMEOUT", 0)

	c.RelayMaxIdleConns = intOr(&errs, "MSA_RELAY_MAX_IDLE_CONNS", 0)
	c.RelayMaxIdleConnsPerHost = intOr(&errs, "MSA_RELAY_MAX_IDLE_CONNS_PER_HOST", 0)
	c.RelayIdleConnTimeout = durationOr(&errs, "MSA_RELAY_IDLE_CONN_TIMEOUT", 0)
	c.RelayResponseHeaderTimeout = durationOr(&errs, "MSA_RELAY_RESPONSE_HEADER_TIMEOUT", 0)
	c.RelayTimeout = durationOr(&errs, "MSA_RELAY_TIMEOUT", 0)
	c.RelayProxy = envOr("MSA_RELAY_PROXY", string(relayclient.ProxyOff))
	if _, err := relayclient.ParseProxyMode(c.RelayProxy); err != nil {
		// 非法值报错而不是静默回落 off：写了这个变量的人是明确想让控制面走代理，
		// 静默关掉会让他以为配好了，而故障表现是「relay 不可达」。
		fail("MSA_RELAY_PROXY is invalid: %v", err)
	}

	c.CaptureMode = envOr("MSA_CAPTURE_MODE", string(capture.ModeOff))
	if _, err := capture.ParseMode(c.CaptureMode); err != nil {
		fail("MSA_CAPTURE_MODE is invalid: %v", err)
	}
	c.CaptureMaxBody = intOr(&errs, "MSA_CAPTURE_MAX_BODY", 0)
	c.CaptureMaxEntries = intOr(&errs, "MSA_CAPTURE_MAX_ENTRIES", 0)

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return c, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// listOr 读一个逗号分隔的列表，空项丢掉。
//
// 丢空项是为了容忍 "a,b," 这种尾逗号：留着会变成一个空字符串来源，
// 而空来源永远匹配不上任何 Origin，看起来像配了其实没生效。
func listOr(key string) []string {
	var out []string
	for _, part := range strings.Split(os.Getenv(key), ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func intOr(errs *[]error, key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s must be an integer: %q", key, raw))
		return fallback
	}
	return v
}

func durationOr(errs *[]error, key string, fallback time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s must be a duration like 30s or 5m: %q", key, raw))
		return fallback
	}
	return v
}

func boolOr(errs *[]error, key string, fallback bool) bool {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s must be a boolean: %q", key, raw))
		return fallback
	}
	return v
}
