package config

import (
	"strings"
	"testing"
)

// 默认不设上限：0 表示沿用驱动默认。
//
// 不在这里给一个具体默认值，是因为本服务可能与调度层、配置中心共用一台 PG，
// 单方面抬高上限只会把连接耗尽点挪到别人身上。
func TestPGMaxConnsDefaultsToDriverDefault(t *testing.T) {
	setRequired(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.PGMaxConns != 0 {
		t.Errorf("PGMaxConns = %d, want 0", c.PGMaxConns)
	}
}

func TestPGMaxConnsIsRead(t *testing.T) {
	setRequired(t)
	t.Setenv("MSA_PG_MAX_CONNS", "16")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.PGMaxConns != 16 {
		t.Errorf("PGMaxConns = %d, want 16", c.PGMaxConns)
	}
}

// 负值必须在启动期报错。
//
// 静默取默认的话，写负数的人会以为自己设成了「不限制」——连接池没有那个
// 语义，而症状要到 PG 侧连接耗尽时才显现。
func TestNegativePGMaxConnsIsRejected(t *testing.T) {
	setRequired(t)
	t.Setenv("MSA_PG_MAX_CONNS", "-1")
	_, err := Load()
	if err == nil {
		t.Fatal("负的池上限被接受了")
	}
	if !strings.Contains(err.Error(), "MSA_PG_MAX_CONNS") {
		t.Errorf("错误没指出是哪个变量: %v", err)
	}
}
