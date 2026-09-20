package main

import (
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/config"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
)

// 控制面的连接层配置同样要逐字段搬。漏一个字段的症状与出站那边一样：
// 运维改了环境变量毫无效果，而看配置看不出生效的是内置默认。
func TestRelayOptionsCarriesEveryField(t *testing.T) {
	got := relayOptions(config.Config{
		RelayMaxIdleConns:          11,
		RelayMaxIdleConnsPerHost:   22,
		RelayIdleConnTimeout:       33 * time.Second,
		RelayResponseHeaderTimeout: 44 * time.Second,
		RelayTimeout:               55 * time.Second,
		RelayProxy:                 string(relayclient.ProxyEnvironment),
	})
	want := relayclient.Options{
		MaxIdleConns:          11,
		MaxIdleConnsPerHost:   22,
		IdleConnTimeout:       33 * time.Second,
		ResponseHeaderTimeout: 44 * time.Second,
		Timeout:               55 * time.Second,
		Proxy:                 relayclient.ProxyEnvironment,
	}
	if got != want {
		t.Fatalf("搬运结果 = %+v, want %+v", got, want)
	}
}

// 负值表示显式不设限，必须原样搬过去。
func TestRelayOptionsPreservesNegatives(t *testing.T) {
	got := relayOptions(config.Config{
		RelayIdleConnTimeout:       -1,
		RelayResponseHeaderTimeout: -1,
		RelayTimeout:               -1,
	})
	if got.IdleConnTimeout >= 0 || got.ResponseHeaderTimeout >= 0 || got.Timeout >= 0 {
		t.Fatalf("负值被吃掉了：%+v，显式不设限就设不了了", got)
	}
}

// 空配置取 off 而不是空串：空串会让 newTransport 走「不等于 environment」
// 那一支，行为恰好正确，但 Options 上留一个非法值会在下次有人加第三态时踩坑。
func TestRelayOptionsDefaultsToProxyOff(t *testing.T) {
	if got := relayOptions(config.Config{}).Proxy; got != relayclient.ProxyOff {
		t.Errorf("默认代理策略 = %q，想要 %q", got, relayclient.ProxyOff)
	}
}

// 控制面与出站两组配置不得互相串。串了的症状是改一边影响另一边，
// 而两层的合理取值差一个数量级。
func TestRelayOptionsIgnoresOutboundFields(t *testing.T) {
	got := relayOptions(config.Config{
		MaxIdleConns:          999,
		MaxIdleConnsPerHost:   999,
		IdleConnTimeout:       999 * time.Second,
		ResponseHeaderTimeout: 999 * time.Second,
	})
	if got != (relayclient.Options{Proxy: relayclient.ProxyOff}) {
		t.Fatalf("出站字段漏进了控制面参数：%+v", got)
	}
}
