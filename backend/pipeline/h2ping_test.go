package pipeline_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/pipeline"
)

// 默认值必须真的配上，且两个都为正。
//
// 只配一半是无意义的状态：光有探测间隔没有失联判定，PING 发出去永远
// 等不到结论；反过来只有失联判定则永远不会发 PING。
func TestH2PingDefaultsAreConfigured(t *testing.T) {
	tr := transportOf(t, pipeline.NewHTTPClient(pipeline.TransportOptions{}))

	if tr.HTTP2 == nil {
		t.Fatal("Transport.HTTP2 为 nil：不配探测，死连接只能等被动超时")
	}
	if tr.HTTP2.SendPingTimeout <= 0 {
		t.Errorf("SendPingTimeout = %v，不为正就永远不会发 PING", tr.HTTP2.SendPingTimeout)
	}
	if tr.HTTP2.PingTimeout <= 0 {
		t.Errorf("PingTimeout = %v，不为正就永远等不到失联结论", tr.HTTP2.PingTimeout)
	}
}

// 最坏检出耗时（两个超时之和）必须显著小于被动的响应头超时，
// 否则被动超时先触发、探测等于没配。
func TestH2PingIsFasterThanPassiveTimeout(t *testing.T) {
	tr := transportOf(t, pipeline.NewHTTPClient(pipeline.TransportOptions{}))
	if tr.HTTP2 == nil {
		t.Fatal("Transport.HTTP2 为 nil")
	}
	worst := tr.HTTP2.SendPingTimeout + tr.HTTP2.PingTimeout
	if worst >= tr.ResponseHeaderTimeout {
		t.Fatalf("最坏检出 %v >= ResponseHeaderTimeout %v，"+
			"被动超时会先触发，主动探测白配", worst, tr.ResponseHeaderTimeout)
	}
}

func TestH2PingExplicitValues(t *testing.T) {
	tr := transportOf(t, pipeline.NewHTTPClient(pipeline.TransportOptions{
		H2SendPingTimeout: 3 * time.Second,
		H2PingTimeout:     4 * time.Second,
	}))
	if tr.HTTP2 == nil {
		t.Fatal("Transport.HTTP2 为 nil")
	}
	if tr.HTTP2.SendPingTimeout != 3*time.Second {
		t.Errorf("SendPingTimeout = %v，要 3s", tr.HTTP2.SendPingTimeout)
	}
	if tr.HTTP2.PingTimeout != 4*time.Second {
		t.Errorf("PingTimeout = %v，要 4s", tr.HTTP2.PingTimeout)
	}
}

// 负值表示显式关闭，且任一为负就整个不配。
func TestH2PingNegativeDisables(t *testing.T) {
	cases := []struct {
		name       string
		send, pong time.Duration
	}{
		{"两个都关", -1, -1},
		{"只关探测间隔", -1, 5 * time.Second},
		{"只关失联判定", 5 * time.Second, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := transportOf(t, pipeline.NewHTTPClient(pipeline.TransportOptions{
				H2SendPingTimeout: c.send,
				H2PingTimeout:     c.pong,
			}))
			if tr.HTTP2 != nil {
				t.Fatalf("HTTP2 = %+v，任一为负就该整个不配——"+
					"只配一半的探测永远得不出结论", tr.HTTP2)
			}
		})
	}
}

// 探测配置不改协议协商：是否走 h2 由 TLS 决定，
// 这一层只管走了之后死连接能不能被探出来。
func TestH2PingDoesNotChangeProtocolNegotiation(t *testing.T) {
	base := http.DefaultTransport.(*http.Transport)
	tr := transportOf(t, pipeline.NewHTTPClient(pipeline.TransportOptions{}))
	if tr.ForceAttemptHTTP2 != base.ForceAttemptHTTP2 {
		t.Errorf("ForceAttemptHTTP2 = %v，被改动了；本轮只加探测不改协商",
			tr.ForceAttemptHTTP2)
	}
}
