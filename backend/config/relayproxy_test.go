package config

import (
	"strings"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/relayclient"
)

// 控制面连接层的五个变量不配时必须留零值：默认值只在 relayclient 一处写。
func TestRelayConnectionDefaultsStayZero(t *testing.T) {
	setRequired(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.RelayMaxIdleConns != 0 || c.RelayMaxIdleConnsPerHost != 0 ||
		c.RelayIdleConnTimeout != 0 || c.RelayResponseHeaderTimeout != 0 ||
		c.RelayTimeout != 0 {
		t.Errorf("控制面连接层默认应当留零值：%d/%d/%v/%v/%v",
			c.RelayMaxIdleConns, c.RelayMaxIdleConnsPerHost,
			c.RelayIdleConnTimeout, c.RelayResponseHeaderTimeout, c.RelayTimeout)
	}
}

func TestRelayConnectionVarsParsed(t *testing.T) {
	setRequired(t)
	t.Setenv("MSA_RELAY_MAX_IDLE_CONNS", "128")
	t.Setenv("MSA_RELAY_MAX_IDLE_CONNS_PER_HOST", "16")
	t.Setenv("MSA_RELAY_IDLE_CONN_TIMEOUT", "45s")
	t.Setenv("MSA_RELAY_RESPONSE_HEADER_TIMEOUT", "3s")
	t.Setenv("MSA_RELAY_TIMEOUT", "20s")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.RelayMaxIdleConns != 128 || c.RelayMaxIdleConnsPerHost != 16 {
		t.Errorf("空闲连接数 = %d/%d, want 128/16",
			c.RelayMaxIdleConns, c.RelayMaxIdleConnsPerHost)
	}
	if c.RelayIdleConnTimeout != 45*time.Second ||
		c.RelayResponseHeaderTimeout != 3*time.Second ||
		c.RelayTimeout != 20*time.Second {
		t.Errorf("超时 = %v/%v/%v, want 45s/3s/20s", c.RelayIdleConnTimeout,
			c.RelayResponseHeaderTimeout, c.RelayTimeout)
	}
}

// 控制面与出站两组变量必须互不影响：配一边不能改到另一边。
// 名字只差一个前缀，抄错的后果是改了控制面却动了数据面的连接池。
func TestRelayVarsDoNotLeakIntoOutbound(t *testing.T) {
	setRequired(t)
	t.Setenv("MSA_RELAY_MAX_IDLE_CONNS", "128")
	t.Setenv("MSA_RELAY_RESPONSE_HEADER_TIMEOUT", "3s")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MaxIdleConns != 0 || c.ResponseHeaderTimeout != 0 {
		t.Errorf("控制面变量串进了出站字段：%d/%v", c.MaxIdleConns, c.ResponseHeaderTimeout)
	}
}

// 代理策略默认 off：控制面的 baseURL 在集群里是服务名，而这类主机名会命中
// HTTP_PROXY，于是调度调用被送去一个不认识它的外网代理。
func TestRelayProxyDefaultsToOff(t *testing.T) {
	setRequired(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.RelayProxy != string(relayclient.ProxyOff) {
		t.Errorf("RelayProxy = %q，想要 %q", c.RelayProxy, relayclient.ProxyOff)
	}
}

func TestRelayProxyEnvironmentAccepted(t *testing.T) {
	setRequired(t)
	t.Setenv("MSA_RELAY_PROXY", "environment")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.RelayProxy != string(relayclient.ProxyEnvironment) {
		t.Errorf("RelayProxy = %q，想要 %q", c.RelayProxy, relayclient.ProxyEnvironment)
	}
}

// 非法值必须是启动错误。静默回落 off 会让明确想让控制面走代理的人以为配好了，
// 而故障表现只是「relay 不可达」。
func TestInvalidRelayProxyFailsStartup(t *testing.T) {
	for _, bad := range []string{"on", "true", "ENVIRONMENT", "env"} {
		t.Run(bad, func(t *testing.T) {
			setRequired(t)
			t.Setenv("MSA_RELAY_PROXY", bad)
			_, err := Load()
			if err == nil {
				t.Fatalf("MSA_RELAY_PROXY=%q 竟然通过了", bad)
			}
			if !strings.Contains(err.Error(), "MSA_RELAY_PROXY") {
				t.Errorf("错误没指明是哪个变量：%v", err)
			}
		})
	}
}

// 负的控制面超时是「显式不设限」，必须能配进来。
func TestNegativeRelayTimeoutsAccepted(t *testing.T) {
	setRequired(t)
	t.Setenv("MSA_RELAY_RESPONSE_HEADER_TIMEOUT", "-1s")
	t.Setenv("MSA_RELAY_TIMEOUT", "-1s")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.RelayResponseHeaderTimeout >= 0 || c.RelayTimeout >= 0 {
		t.Errorf("负值被吃掉了：%v/%v", c.RelayResponseHeaderTimeout, c.RelayTimeout)
	}
}
