package main

import (
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/config"
	"github.com/aceaura/model-surge-agent/backend/pipeline"
)

// 连接层配置必须逐字段搬到出站参数上。
//
// 漏一个字段编译不会报错、测试也不会红，症状是运维改了环境变量毫无效果——
// 而且看配置看不出实际生效的是内置默认。每个字段给一个互不相同的值，
// 搬错位置也能看出来。
func TestTransportOptionsCarriesEveryField(t *testing.T) {
	got := transportOptions(config.Config{
		MaxIdleConns:          11,
		MaxIdleConnsPerHost:   22,
		IdleConnTimeout:       33 * time.Second,
		ResponseHeaderTimeout: 44 * time.Second,
		H2SendPingTimeout:     55 * time.Second,
		H2PingTimeout:         66 * time.Second,
	})
	want := pipeline.TransportOptions{
		MaxIdleConns:          11,
		MaxIdleConnsPerHost:   22,
		IdleConnTimeout:       33 * time.Second,
		ResponseHeaderTimeout: 44 * time.Second,
		H2SendPingTimeout:     55 * time.Second,
		H2PingTimeout:         66 * time.Second,
	}
	if got != want {
		t.Fatalf("搬运结果 = %+v, want %+v", got, want)
	}
}

// 负值表示显式关闭，必须原样搬过去而不是被归一成零。
func TestTransportOptionsPreservesNegatives(t *testing.T) {
	got := transportOptions(config.Config{
		ResponseHeaderTimeout: -1,
		H2SendPingTimeout:     -1,
		H2PingTimeout:         -1,
	})
	if got.ResponseHeaderTimeout >= 0 || got.H2SendPingTimeout >= 0 || got.H2PingTimeout >= 0 {
		t.Fatalf("负值被吃掉了：%+v，显式关闭就关不掉了", got)
	}
}
