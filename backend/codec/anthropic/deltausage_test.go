package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// message_delta 帧的 usage 是官方 MessageDeltaUsage：没有 cache_creation
// 对象与 inference_geo——那两个只属于 message_start / 非流式响应的完整
// Usage。同族「上游非流式→客户端流式」转换必然把带明细的聚合 usage 送进
// EvMessageDelta，复用完整 DTO 就是往帧里写官方 schema 没有的键。
func TestMessageDeltaWritesOnlyOfficialUsageKeys(t *testing.T) {
	usage := ir.Usage{
		InputTokens:            70,
		OutputTokens:           20,
		CacheReadTokens:        30,
		CacheWriteTokens:       25,
		CacheWrite5mTokens:     10,
		CacheWrite1hTokens:     15,
		CacheWriteDetailsKnown: true,
		WebSearchRequests:      2,
		InferenceGeo:           "us",
	}
	enc := newStreamEncoder()
	var wire strings.Builder
	feed := func(ev ir.Event) {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("Encode(%s): %v", ev.Type, err)
		}
		for _, f := range frames {
			wire.Write(f)
		}
	}
	u := usage
	feed(ir.Event{Type: ir.EvMessageStart, MessageID: "msg_1", Model: "m",
		ServiceTier: "standard", Usage: &u})
	feed(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}})
	feed(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "正文"})
	feed(ir.Event{Type: ir.EvBlockStop, Index: 0})
	u2 := usage
	feed(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Usage: &u2})
	feed(ir.Event{Type: ir.EvMessageStop})
	for _, f := range enc.Finish() {
		wire.Write(f)
	}
	s := wire.String()

	start := frameBody(t, s, "message_start")
	delta := frameBody(t, s, "message_delta")

	// message_start 仍是完整 Usage：TTL 明细与地理回显在这里承担。
	if !strings.Contains(start, `"cache_creation":{`) {
		t.Fatalf("message_start 缺 cache_creation 明细：%s", start)
	}
	if !strings.Contains(start, `"inference_geo":"us"`) {
		t.Fatalf("message_start 缺 inference_geo：%s", start)
	}
	// message_delta 只写官方六键之内的形状。
	if !strings.Contains(delta, `"cache_creation_input_tokens":25`) {
		t.Fatalf("message_delta 缺平铺写入量：%s", delta)
	}
	if !strings.Contains(delta, `"cache_read_input_tokens":30`) {
		t.Fatalf("message_delta 缺平铺读取量：%s", delta)
	}
	if !strings.Contains(delta, `"server_tool_use":{`) {
		t.Fatalf("message_delta 缺托管工具次数：%s", delta)
	}
	for _, banned := range []string{`"cache_creation":{`, "inference_geo", "service_tier"} {
		if strings.Contains(delta, banned) {
			t.Fatalf("message_delta 带出了官方没有的键 %s：%s", banned, delta)
		}
	}
}

// frameBody 取指定事件名单帧的 data 行。
func frameBody(t *testing.T, s, event string) string {
	t.Helper()
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data:") && strings.Contains(line, `"type":"`+event+`"`) {
			return line
		}
	}
	t.Fatalf("没找到 %s 帧：%s", event, s)
	return ""
}
