package config

import (
	"strings"
	"testing"
	"time"
)

// setRequired 只填必需项，其余走默认值。
func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("MSA_PG_DSN", "postgres://msa:msa@localhost:5432/msa")
	t.Setenv("MSA_RELAY_BASE_URL", "http://relay:8080/")
	t.Setenv("MSA_RELAY_DISPATCH_KEY", "dispatch-key")
	t.Setenv("MSA_ADMIN_KEY", "admin-key")
}

func TestLoadDefaults(t *testing.T) {
	setRequired(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Listen != ":8080" {
		t.Errorf("listen = %q", c.Listen)
	}
	// 尾斜杠必须去掉，否则拼出的路径会带双斜杠。
	if c.RelayBaseURL != "http://relay:8080" {
		t.Errorf("relay base url = %q", c.RelayBaseURL)
	}
	if c.MaxAttempts != 3 || c.FirstTokenTimeout != 60*time.Second || c.IdleTimeout != 120*time.Second {
		t.Errorf("retry defaults = %+v", c)
	}
	if c.OutboxInterval != time.Second || c.OutboxMaxAttempts != 20 {
		t.Errorf("outbox defaults = %+v", c)
	}
	if !c.EstimateUsage || !c.AccessLog {
		t.Errorf("boolean defaults = %+v", c)
	}
	// Redis 可选：不配就没有缓存，服务照常启动。
	if c.RedisAddr != "" {
		t.Errorf("redis addr = %q, want empty", c.RedisAddr)
	}
}

// 改配置的人在容器日志里通常只看得到第一条错误，
// 逐个报错会让他来回重启好几轮才配对。
func TestLoadReportsEveryMissingVariableAtOnce(t *testing.T) {
	t.Setenv("MSA_PG_DSN", "")
	t.Setenv("MSA_RELAY_BASE_URL", "")
	t.Setenv("MSA_RELAY_DISPATCH_KEY", "")
	t.Setenv("MSA_ADMIN_KEY", "")

	_, err := Load()
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{
		"MSA_PG_DSN", "MSA_RELAY_BASE_URL", "MSA_RELAY_DISPATCH_KEY", "MSA_ADMIN_KEY",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %s: %v", want, err)
		}
	}
}

// 两把密钥职责不同：相同则任何能读管理面的人都能冒充本服务发调度请求。
func TestAdminKeyMustDifferFromDispatchKey(t *testing.T) {
	setRequired(t)
	t.Setenv("MSA_ADMIN_KEY", "same-key")
	t.Setenv("MSA_RELAY_DISPATCH_KEY", "same-key")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("err = %v, want a distinctness complaint", err)
	}
}

func TestMalformedValuesAreRejected(t *testing.T) {
	cases := []struct{ key, value string }{
		{"MSA_REDIS_DB", "abc"},
		{"MSA_CACHE_TTL", "60"},
		{"MSA_MAX_ATTEMPTS", "many"},
		{"MSA_FIRST_TOKEN_TIMEOUT", "1 minute"},
		{"MSA_ESTIMATE_USAGE", "yes-please"},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			setRequired(t)
			t.Setenv(tc.key, tc.value)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("err = %v, want a complaint about %s", err, tc.key)
			}
		})
	}
}

func TestMaxAttemptsMustAllowAtLeastOneTry(t *testing.T) {
	setRequired(t)
	t.Setenv("MSA_MAX_ATTEMPTS", "0")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "MSA_MAX_ATTEMPTS") {
		t.Fatalf("err = %v", err)
	}
}

func TestOptionalOverridesTakeEffect(t *testing.T) {
	setRequired(t)
	t.Setenv("MSA_LISTEN", "127.0.0.1:9000")
	t.Setenv("MSA_REDIS_ADDR", "redis:6379")
	t.Setenv("MSA_REDIS_DB", "3")
	t.Setenv("MSA_CACHE_TTL", "30s")
	t.Setenv("MSA_ESTIMATE_USAGE", "false")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Listen != "127.0.0.1:9000" || c.RedisAddr != "redis:6379" || c.RedisDB != 3 {
		t.Errorf("config = %+v", c)
	}
	if c.CacheTTL != 30*time.Second || c.EstimateUsage {
		t.Errorf("config = %+v", c)
	}
}
