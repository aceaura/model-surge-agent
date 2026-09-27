package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次24：官方 anthropic 把「实际执行档位回显」放在 usage.service_tier
// （Usage.service_tier），Message 顶层没有这个键（据 anthropic-sdk-typescript：
// Message 仅 id/type/role/model/content/stop_reason/stop_sequence/stop_details/
// container/usage）。此前本网关误把 service_tier 建模在 wireResponse /
// streamMsg 顶层，导致：对真实 Anthropic 上游，解码读一个永不出现的顶层键、
// 又没建 usage.service_tier，执行档位（standard/priority/batch，关乎计费与
// 限流归因）被 json.Unmarshal 静默吞掉；对 anthropic 客户端，编码又把它写在
// 非标准的顶层，官方 SDK 读 usage.service_tier 得到 null。这组测试把档位
// 钉死在 usage 下，并确认顶层键不再被读写。

// dataOf 从 SSE 帧里取出 data: 后的 JSON 负载。
func dataOf(t *testing.T, frame []byte) map[string]json.RawMessage {
	t.Helper()
	s := string(frame)
	i := strings.Index(s, "data: ")
	if i < 0 {
		t.Fatalf("帧里没有 data: 行：%q", s)
	}
	payload := strings.TrimSpace(s[i+len("data: "):])
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &m); err != nil {
		t.Fatalf("data 负载解不开：%v（%q）", err, payload)
	}
	return m
}

func usageTier(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var u map[string]json.RawMessage
	if err := json.Unmarshal(raw, &u); err != nil {
		t.Fatalf("usage 解不开：%v", err)
	}
	var tier string
	if v, ok := u["service_tier"]; ok {
		_ = json.Unmarshal(v, &tier)
	}
	return tier
}

// 解码：非流式响应从 usage.service_tier 读出执行档位。
func TestServiceTierDecodedFromUsageNonStreaming(t *testing.T) {
	body := []byte(`{"id":"m1","type":"message","role":"assistant","model":"m",` +
		`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":1,"output_tokens":1,"service_tier":"priority"}}`)
	resp, err := DecodeResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.ServiceTier != "priority" {
		t.Fatalf("usage.service_tier 没读进 IR，得到 %q，想要 priority", resp.ServiceTier)
	}
}

// 解码：非流式响应里「顶层」service_tier（旧错误位置）不再被读取——官方顶层
// 没有这个键，读它等于凭空捏造来源；真实值只在 usage 下。
func TestServiceTierTopLevelIgnoredNonStreaming(t *testing.T) {
	body := []byte(`{"id":"m1","type":"message","role":"assistant","model":"m",` +
		`"service_tier":"priority","content":[{"type":"text","text":"hi"}],` +
		`"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	resp, err := DecodeResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.ServiceTier != "" {
		t.Fatalf("顶层 service_tier 不该被读取，却得到 %q", resp.ServiceTier)
	}
}

// 解码：流式 message_start 从 message.usage.service_tier 读出执行档位。
func TestServiceTierDecodedFromUsageStreaming(t *testing.T) {
	d := newStreamDecoder()
	evs, err := d.Feed("message_start",
		`{"type":"message_start","message":{"id":"m1","model":"m","role":"assistant",`+
			`"content":[],"usage":{"input_tokens":1,"output_tokens":0,"service_tier":"batch"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	var got string
	for _, ev := range evs {
		if ev.Type == ir.EvMessageStart {
			got = ev.ServiceTier
		}
	}
	if got != "batch" {
		t.Fatalf("message_start 的 usage.service_tier 没读进事件，得到 %q，想要 batch", got)
	}
}

// 编码：非流式响应把执行档位写进 usage.service_tier，且顶层不再出现该键。
func TestServiceTierEncodedIntoUsageNonStreaming(t *testing.T) {
	out, err := EncodeResponse(&ir.Response{
		ID: "m1", Model: "m", ServiceTier: "standard",
		Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}},
		Usage:   ir.Usage{InputTokens: 1, OutputTokens: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatal(err)
	}
	if _, ok := top["service_tier"]; ok {
		t.Errorf("顶层不该再写 service_tier：%s", out)
	}
	u, ok := top["usage"]
	if !ok {
		t.Fatalf("响应缺 usage：%s", out)
	}
	tier := usageTier(t, u)
	if tier != "standard" {
		t.Errorf("usage.service_tier 没写出，得到 %q，想要 standard：%s", tier, out)
	}
}

// 编码：流式 message_start 把执行档位写进 message.usage.service_tier，
// 且 message 顶层不再出现该键。
func TestServiceTierEncodedIntoUsageStreaming(t *testing.T) {
	e := newStreamEncoder()
	frames, err := e.Encode(ir.Event{
		Type: ir.EvMessageStart, MessageID: "m1", Model: "m", ServiceTier: "standard",
		Usage: &ir.Usage{InputTokens: 1, OutputTokens: 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) == 0 {
		t.Fatal("没有产出 message_start 帧")
	}
	top := dataOf(t, frames[0])
	msgRaw, ok := top["message"]
	if !ok {
		t.Fatalf("message_start 帧缺 message：%s", frames[0])
	}
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(msgRaw, &msg); err != nil {
		t.Fatal(err)
	}
	if _, ok := msg["service_tier"]; ok {
		t.Errorf("message 顶层不该再写 service_tier：%s", frames[0])
	}
	u, ok := msg["usage"]
	if !ok {
		t.Fatalf("message_start 缺 usage：%s", frames[0])
	}
	tier := usageTier(t, u)
	if tier != "standard" {
		t.Errorf("message.usage.service_tier 没写出，得到 %q，想要 standard：%s", tier, frames[0])
	}
}
