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
)

type Config struct {
	Listen string

	PGDSN         string
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

	OutboxInterval    time.Duration
	OutboxMaxAttempts int

	EstimateUsage bool
	AccessLog     bool
	LogRetention  time.Duration

	// CORSOrigins 是允许跨域的来源。空或含 "*" 表示放开所有来源，
	// 此时不发 Allow-Credentials——浏览器拒绝 `*` 与凭据并存。
	CORSOrigins []string
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

	c.RedisDB = intOr(&errs, "MSA_REDIS_DB", 0)
	c.CacheTTL = durationOr(&errs, "MSA_CACHE_TTL", time.Minute)
	c.MaxAttempts = intOr(&errs, "MSA_MAX_ATTEMPTS", 3)
	if c.MaxAttempts < 1 {
		fail("MSA_MAX_ATTEMPTS must be at least 1")
	}
	c.FirstTokenTimeout = durationOr(&errs, "MSA_FIRST_TOKEN_TIMEOUT", 60*time.Second)
	c.IdleTimeout = durationOr(&errs, "MSA_IDLE_TIMEOUT", 120*time.Second)
	c.OutboxInterval = durationOr(&errs, "MSA_OUTBOX_INTERVAL", time.Second)
	c.OutboxMaxAttempts = intOr(&errs, "MSA_OUTBOX_MAX_ATTEMPTS", 20)
	c.EstimateUsage = boolOr(&errs, "MSA_ESTIMATE_USAGE", true)
	c.AccessLog = boolOr(&errs, "MSA_ACCESS_LOG", true)
	c.LogRetention = durationOr(&errs, "MSA_LOG_RETENTION", 14*24*time.Hour)
	c.CORSOrigins = listOr("MSA_CORS_ORIGINS")

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
