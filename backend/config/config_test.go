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

// TestCORSOriginsDefaultToOpen 钉住不配就是放开所有来源。
//
// 默认放开而不是默认全拦：数据面本来就必须部署在受信网络或反代之后
// （它不做自身鉴权），再加一层 CORS 默认拦只会让浏览器端的调试莫名失败。
func TestCORSOriginsDefaultToOpen(t *testing.T) {
	setRequired(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.CORSOrigins) != 0 {
		t.Errorf("cors origins = %v, want empty (open)", c.CORSOrigins)
	}
}

// TestCORSOriginsSplitAndTrim 钉住列表解析。
//
// 空项必须丢掉：尾逗号留下的空来源永远匹配不上任何 Origin，
// 看起来像配了其实没生效。
func TestCORSOriginsSplitAndTrim(t *testing.T) {
	setRequired(t)
	t.Setenv("MSA_CORS_ORIGINS", " https://a.example.com , https://b.example.com ,")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"https://a.example.com", "https://b.example.com"}
	if len(c.CORSOrigins) != len(want) {
		t.Fatalf("cors origins = %v, want %v", c.CORSOrigins, want)
	}
	for i, w := range want {
		if c.CORSOrigins[i] != w {
			t.Errorf("origin[%d] = %q, want %q", i, c.CORSOrigins[i], w)
		}
	}
}

// 出站连接层的四个变量不配时必须留零值：默认值只在 pipeline 一处写，
// 两边各写一份迟早漂移，而漂移之后看配置看不出实际生效的是哪个。
func TestOutboundConnectionDefaultsStayZero(t *testing.T) {
	setRequired(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MaxIdleConns != 0 || c.MaxIdleConnsPerHost != 0 ||
		c.IdleConnTimeout != 0 || c.ResponseHeaderTimeout != 0 {
		t.Errorf("连接层默认应当留零值交给 pipeline：%d/%d/%v/%v",
			c.MaxIdleConns, c.MaxIdleConnsPerHost,
			c.IdleConnTimeout, c.ResponseHeaderTimeout)
	}
}

func TestOutboundConnectionVarsParsed(t *testing.T) {
	setRequired(t)
	t.Setenv("MSA_MAX_IDLE_CONNS", "512")
	t.Setenv("MSA_MAX_IDLE_CONNS_PER_HOST", "64")
	t.Setenv("MSA_IDLE_CONN_TIMEOUT", "30s")
	t.Setenv("MSA_RESPONSE_HEADER_TIMEOUT", "5m")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MaxIdleConns != 512 || c.MaxIdleConnsPerHost != 64 {
		t.Errorf("空闲连接数 = %d/%d, want 512/64", c.MaxIdleConns, c.MaxIdleConnsPerHost)
	}
	if c.IdleConnTimeout != 30*time.Second || c.ResponseHeaderTimeout != 5*time.Minute {
		t.Errorf("超时 = %v/%v, want 30s/5m", c.IdleConnTimeout, c.ResponseHeaderTimeout)
	}
}

// 负的响应头超时是「显式不设限」，必须能配进来而不是被当成非法值。
func TestNegativeResponseHeaderTimeoutAccepted(t *testing.T) {
	setRequired(t)
	t.Setenv("MSA_RESPONSE_HEADER_TIMEOUT", "-1s")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ResponseHeaderTimeout >= 0 {
		t.Errorf("ResponseHeaderTimeout = %v，负值应当原样透传表示不设限",
			c.ResponseHeaderTimeout)
	}
}

// 超出 int64 纳秒的时长必须进启动错误，而不是回绕成一个极小的正值
// ——回绕后每个请求都会被掐断，症状是「上游全挂了」。
func TestOverflowingDurationIsAStartupError(t *testing.T) {
	setRequired(t)
	t.Setenv("MSA_RESPONSE_HEADER_TIMEOUT", "2562048h")
	if _, err := Load(); err == nil {
		t.Fatal("溢出的时长被接受了")
	} else if !strings.Contains(err.Error(), "MSA_RESPONSE_HEADER_TIMEOUT") {
		t.Errorf("错误没指出是哪个变量：%v", err)
	}
}
