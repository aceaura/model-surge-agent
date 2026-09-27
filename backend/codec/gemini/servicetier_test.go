package gemini

import (
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// gemini 上游在 usageMetadata.serviceTier 回显实际执行档位（Output only，enum
// unspecified/standard/flex/priority）。此前 wireUsage 未建模该键、两条解码路径
// 都不读，导致 gemini 上游时 IR.Response.ServiceTier 恒空：客户端拿不到实际
// 计费/优先级档位、也没有任何注记，与 anthropic/chat/responses 三族都捕获
// tier echo 不对称。以下钉住解码保全与 unspecified 的零值归一。

// 非流式：usageMetadata.serviceTier 原值进 IR.Response.ServiceTier。
func TestServiceTierEchoDecodedNonStreaming(t *testing.T) {
	for _, tier := range []string{"standard", "flex", "priority"} {
		body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"ok"}]}}],` +
			`"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"serviceTier":"` + tier + `"}}`)
		resp, _, err := DecodeResponseLossy(body)
		if err != nil {
			t.Fatalf("DecodeResponseLossy(%s): %v", tier, err)
		}
		if resp.ServiceTier != tier {
			t.Errorf("serviceTier=%s 没原值进 IR，得到 %q", tier, resp.ServiceTier)
		}
	}
}

// unspecified 是 enum 零值（官方注「Default service tier, which is standard」），
// 与「上游没给」不可分，且不是客户端能识别的标准枚举，故归一为缺席，不原样透传。
func TestServiceTierUnspecifiedTreatedAsAbsent(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"ok"}]}}],` +
		`"usageMetadata":{"promptTokenCount":10,"serviceTier":"unspecified"}}`)
	resp, _, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if resp.ServiceTier != "" {
		t.Errorf("unspecified 应归零为缺席，得到 %q", resp.ServiceTier)
	}
}

// 上游没给 serviceTier：留空，不发明档位。
func TestServiceTierAbsentStaysEmpty(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"ok"}]}}],` +
		`"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5}}`)
	resp, _, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if resp.ServiceTier != "" {
		t.Errorf("缺席时不应有档位，得到 %q", resp.ServiceTier)
	}
}

// 流式：档位随 usageMetadata 到达，经终止 EvMessageDelta 的 ServiceTier 带回，
// 聚合后落进 IR.Response.ServiceTier（与非流式同口径）。
func TestServiceTierEchoDecodedStreaming(t *testing.T) {
	dec := newStreamDecoder()
	if _, err := dec.Feed("", `{"candidates":[{"content":{"parts":[{"text":"hi"}]}}],`+
		`"usageMetadata":{"candidatesTokenCount":10,"serviceTier":"priority"}}`); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	var agg ir.Aggregator
	var sawOnDelta bool
	for _, ev := range dec.Finish() {
		if ev.Type == ir.EvMessageDelta && ev.ServiceTier != "" {
			sawOnDelta = true
		}
		agg.Add(ev)
	}
	if !sawOnDelta {
		t.Errorf("终止 EvMessageDelta 应带出 serviceTier")
	}
	if got := agg.Response().ServiceTier; got != "priority" {
		t.Errorf("流式聚合后档位应为 priority，得到 %q", got)
	}
}

// 流式 unspecified 同样归零：终止帧不带档位，聚合结果为空。
func TestServiceTierUnspecifiedNotEmittedInStream(t *testing.T) {
	dec := newStreamDecoder()
	if _, err := dec.Feed("", `{"candidates":[{"content":{"parts":[{"text":"hi"}]}}],`+
		`"usageMetadata":{"candidatesTokenCount":10,"serviceTier":"unspecified"}}`); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	for _, ev := range dec.Finish() {
		if ev.ServiceTier != "" {
			t.Errorf("unspecified 不应带上事件，得到 %q", ev.ServiceTier)
		}
	}
}
