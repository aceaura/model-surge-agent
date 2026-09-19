package config

import (
	"strings"
	"testing"
	"time"
)

// 未设置时宽限期派生自流超时，而不是一个写死的常量。
//
// 写死会随流超时被调大而重新变得短于它——「默认配置自相矛盾」这个状态
// 正是本轮要消掉的。
func TestShutdownGraceDefaultsToTheLargerStreamTimeout(t *testing.T) {
	setRequired(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// 默认下空闲超时（120s）大于首字超时（60s）。
	if c.ShutdownGrace != 120*time.Second {
		t.Errorf("shutdown grace = %v, want 120s（= max(首字, 空闲)）", c.ShutdownGrace)
	}
	if c.ShutdownLinger != 30*time.Second {
		t.Errorf("shutdown linger = %v, want 30s", c.ShutdownLinger)
	}
	if c.ShutdownFlushTimeout != 10*time.Second {
		t.Errorf("shutdown flush = %v, want 10s", c.ShutdownFlushTimeout)
	}
}

// 派生取的是两者中的较大者，不是其中固定的某一个。
func TestShutdownGraceFollowsWhicheverStreamTimeoutIsLarger(t *testing.T) {
	setRequired(t)
	// 让首字反超空闲：盯着 IdleTimeout 的实现会在这里取到 40s。
	t.Setenv("MSA_FIRST_TOKEN_TIMEOUT", "300s")
	t.Setenv("MSA_IDLE_TIMEOUT", "40s")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ShutdownGrace != 300*time.Second {
		t.Errorf("shutdown grace = %v, want 300s（首字更大）", c.ShutdownGrace)
	}
}

// 显式设小于流超时必须报错：两个值各自合法，组合起来保证截断正当流量。
func TestShutdownGraceBelowStreamTimeoutIsRejected(t *testing.T) {
	setRequired(t)
	t.Setenv("MSA_IDLE_TIMEOUT", "120s")
	t.Setenv("MSA_SHUTDOWN_GRACE", "20s")
	_, err := Load()
	if err == nil {
		t.Fatal("宽限期短于流超时却通过了校验")
	}
	// 错误文本必须点名变量，否则运维不知道改哪个。
	for _, want := range []string{"MSA_SHUTDOWN_GRACE", "MSA_IDLE_TIMEOUT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误文本没提到 %s: %v", want, err)
		}
	}
}

// 恰好相等是允许的：下限是「至少」而不是「大于」。
func TestShutdownGraceEqualToStreamTimeoutIsAllowed(t *testing.T) {
	setRequired(t)
	t.Setenv("MSA_IDLE_TIMEOUT", "90s")
	t.Setenv("MSA_FIRST_TOKEN_TIMEOUT", "30s")
	t.Setenv("MSA_SHUTDOWN_GRACE", "90s")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ShutdownGrace != 90*time.Second {
		t.Errorf("shutdown grace = %v, want 90s", c.ShutdownGrace)
	}
}

// 显式设大于流超时当然可以，且取用户给的那个值。
func TestShutdownGraceAboveStreamTimeoutIsTaken(t *testing.T) {
	setRequired(t)
	t.Setenv("MSA_SHUTDOWN_GRACE", "300s")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ShutdownGrace != 300*time.Second {
		t.Errorf("shutdown grace = %v, want 300s", c.ShutdownGrace)
	}
}

// 显式写 0s 是「不等」的意图，必须被拒而不是被当成未设置从而静默取默认。
//
// 看值判「是否设置」的实现会在这里给出 120s 并放行，
// 于是运维以为自己关掉了等待，实际等满两分钟。
func TestExplicitZeroShutdownGraceIsRejectedNotTreatedAsUnset(t *testing.T) {
	setRequired(t)
	t.Setenv("MSA_SHUTDOWN_GRACE", "0s")
	_, err := Load()
	if err == nil {
		t.Fatal("显式 0s 的宽限期被静默当成了未设置")
	}
	if !strings.Contains(err.Error(), "MSA_SHUTDOWN_GRACE") {
		t.Errorf("错误文本没提到 MSA_SHUTDOWN_GRACE: %v", err)
	}
}

// 清理等待与冲队列预算不接受非正值：那恰好是本轮修掉的两个缺陷的样子。
func TestNonPositiveShutdownBudgetsAreRejected(t *testing.T) {
	cases := []struct {
		key string
		val string
	}{
		{"MSA_SHUTDOWN_LINGER", "0s"},
		{"MSA_SHUTDOWN_LINGER", "-1s"},
		{"MSA_SHUTDOWN_FLUSH_TIMEOUT", "0s"},
		{"MSA_SHUTDOWN_FLUSH_TIMEOUT", "-5s"},
	}
	for _, c := range cases {
		t.Run(c.key+"="+c.val, func(t *testing.T) {
			setRequired(t)
			t.Setenv(c.key, c.val)
			_, err := Load()
			if err == nil {
				t.Fatalf("%s=%s 通过了校验", c.key, c.val)
			}
			if !strings.Contains(err.Error(), c.key) {
				t.Errorf("错误文本没提到 %s: %v", c.key, err)
			}
		})
	}
}

// 关停这三条错误与其它配置错误一起报全，沿用本包既有口径。
func TestShutdownErrorsJoinTheRest(t *testing.T) {
	setRequired(t)
	t.Setenv("MSA_PG_DSN", "")
	t.Setenv("MSA_SHUTDOWN_GRACE", "1s")
	t.Setenv("MSA_SHUTDOWN_LINGER", "0s")
	t.Setenv("MSA_SHUTDOWN_FLUSH_TIMEOUT", "0s")
	_, err := Load()
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{
		"MSA_PG_DSN", "MSA_SHUTDOWN_GRACE",
		"MSA_SHUTDOWN_LINGER", "MSA_SHUTDOWN_FLUSH_TIMEOUT",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误文本没提到 %s: %v", want, err)
		}
	}
}

// 非法时长仍走既有的解析报错路径，不被关停校验吃掉。
func TestMalformedShutdownDurationIsReported(t *testing.T) {
	setRequired(t)
	t.Setenv("MSA_SHUTDOWN_GRACE", "soon")
	_, err := Load()
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "MSA_SHUTDOWN_GRACE") {
		t.Errorf("错误文本没提到 MSA_SHUTDOWN_GRACE: %v", err)
	}
}
