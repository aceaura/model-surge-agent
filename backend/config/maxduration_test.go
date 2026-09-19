package config

import (
	"strings"
	"testing"
	"time"
)

// 判据 20：默认不限。既有部署没配过这个变量，默认收紧会把长生成打断。
func TestMaxRequestDurationDefaultsToUnlimited(t *testing.T) {
	setRequired(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MaxRequestDuration != 0 {
		t.Errorf("MaxRequestDuration = %s，want 0（不限）", c.MaxRequestDuration)
	}
}

// 判据 21：显式值被读进来。
func TestMaxRequestDurationParsed(t *testing.T) {
	setRequired(t)
	t.Setenv("MSA_MAX_REQUEST_DURATION", "10m")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MaxRequestDuration != 10*time.Minute {
		t.Errorf("MaxRequestDuration = %s", c.MaxRequestDuration)
	}
}

// 判据 22：负值拒绝启动，并点明 0 才是关闭的写法。
func TestNegativeMaxRequestDurationFailsStartup(t *testing.T) {
	setRequired(t)
	t.Setenv("MSA_MAX_REQUEST_DURATION", "-1s")
	_, err := Load()
	if err == nil {
		t.Fatal("负值被接受了")
	}
	if !strings.Contains(err.Error(), "MSA_MAX_REQUEST_DURATION") {
		t.Errorf("错误没点名变量：%v", err)
	}
	if !strings.Contains(err.Error(), "use 0") {
		t.Errorf("错误没说怎么关闭：%v", err)
	}
}

// 判据 23：总上限小于单次尝试的超时时拒绝启动。
//
// 不拒的话每个请求都在同一时刻被总上限掐断，重试与单次超时全部失效，
// 而失效方式看上去像上游整体故障。
func TestMaxRequestDurationBelowPerAttemptTimeoutFailsStartup(t *testing.T) {
	setRequired(t)
	t.Setenv("MSA_FIRST_TOKEN_TIMEOUT", "30s")
	t.Setenv("MSA_IDLE_TIMEOUT", "90s")
	t.Setenv("MSA_MAX_REQUEST_DURATION", "60s")
	_, err := Load()
	if err == nil {
		t.Fatal("总上限 60s 小于空闲超时 90s 却启动了")
	}
	if !strings.Contains(err.Error(), "MSA_IDLE_TIMEOUT") {
		t.Errorf("错误没指出下限来自哪个变量：%v", err)
	}
}

// 判据 24：恰好等于较大者的那一档放行——下限是「至少」而不是「大于」。
func TestMaxRequestDurationEqualToBoundIsAccepted(t *testing.T) {
	setRequired(t)
	t.Setenv("MSA_FIRST_TOKEN_TIMEOUT", "30s")
	t.Setenv("MSA_IDLE_TIMEOUT", "90s")
	t.Setenv("MSA_MAX_REQUEST_DURATION", "90s")
	if _, err := Load(); err != nil {
		t.Errorf("Load: %v", err)
	}
}

// 判据 25：关闭（0）时不参与联合校验——否则「不限」会被自己的下限拒掉。
func TestZeroMaxRequestDurationSkipsJointValidation(t *testing.T) {
	setRequired(t)
	t.Setenv("MSA_FIRST_TOKEN_TIMEOUT", "30s")
	t.Setenv("MSA_IDLE_TIMEOUT", "90s")
	t.Setenv("MSA_MAX_REQUEST_DURATION", "0")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MaxRequestDuration != 0 {
		t.Errorf("MaxRequestDuration = %s", c.MaxRequestDuration)
	}
}
