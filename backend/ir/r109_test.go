package ir

import "testing"

// 聚合器收下上游创建时间：EvMessageStart 携带的 Created 落进 Response.Created，
// 非流式化后与时间戳回显同一口径。#71b 已引入该行为，这里钉住防回归：
// 整份响应路径的出站编码器从首帧取 Created，聚合器丢了它，上游的真实
// 创建时间会被代理本地钟顶替。
func TestAggregatorCarriesCreated(t *testing.T) {
	var a Aggregator
	a.Add(Event{Type: EvMessageStart, MessageID: "m1", Model: "m", Created: 1750000000})
	a.Add(Event{Type: EvMessageDelta, StopReason: StopEndTurn})
	a.Add(Event{Type: EvMessageStop})
	resp := a.Response()
	if resp.Created != 1750000000 {
		t.Fatalf("Created = %d, want 1750000000", resp.Created)
	}

	// 上游没给时保持零值：编码器回退本地钟，不伪造。
	var b Aggregator
	b.Add(Event{Type: EvMessageStart, MessageID: "m1", Model: "m"})
	b.Add(Event{Type: EvMessageDelta, StopReason: StopEndTurn})
	b.Add(Event{Type: EvMessageStop})
	if got := b.Response().Created; got != 0 {
		t.Fatalf("零值被伪造：%d", got)
	}
}
