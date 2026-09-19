package config

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/capture"
)

func TestCaptureDefaultsToOff(t *testing.T) {
	setRequired(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.CaptureMode != string(capture.ModeOff) {
		t.Errorf("capture mode = %q, want off —— 捕获会把请求全文留在进程里，"+
			"默认必须是关的", c.CaptureMode)
	}
	// 零值交给 capture 包取内置默认，不在 config 里重复一份。
	if c.CaptureMaxBody != 0 || c.CaptureMaxEntries != 0 {
		t.Errorf("上限默认应为零值：%d / %d", c.CaptureMaxBody, c.CaptureMaxEntries)
	}
}

func TestCaptureModeAcceptsThreeStates(t *testing.T) {
	for _, mode := range []string{"off", "errors", "all"} {
		setRequired(t)
		t.Setenv("MSA_CAPTURE_MODE", mode)
		c, err := Load()
		if err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
		if c.CaptureMode != mode {
			t.Errorf("mode = %q, want %q", c.CaptureMode, mode)
		}
	}
}

// 非法值必须在启动时报错。静默回落到 off 的后果是运维以为捕获开着，
// 等出了故障才发现什么都没留，而那时故障已经过去了。
func TestInvalidCaptureModeFailsStartup(t *testing.T) {
	for _, bad := range []string{"on", "true", "ERRORS", "yes"} {
		setRequired(t)
		t.Setenv("MSA_CAPTURE_MODE", bad)
		_, err := Load()
		if err == nil {
			t.Errorf("MSA_CAPTURE_MODE=%q 静默通过了", bad)
			continue
		}
		if !strings.Contains(err.Error(), "MSA_CAPTURE_MODE") {
			t.Errorf("错误里没指出是哪个变量：%v", err)
		}
	}
}

func TestCaptureLimitsAreReadFromEnv(t *testing.T) {
	setRequired(t)
	t.Setenv("MSA_CAPTURE_MODE", "all")
	t.Setenv("MSA_CAPTURE_MAX_BODY", "4096")
	t.Setenv("MSA_CAPTURE_MAX_ENTRIES", "8")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.CaptureMaxBody != 4096 || c.CaptureMaxEntries != 8 {
		t.Errorf("上限没读进来：%d / %d", c.CaptureMaxBody, c.CaptureMaxEntries)
	}
}
