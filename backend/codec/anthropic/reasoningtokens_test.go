package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次26：官方 anthropic 的 Usage 与 MessageDeltaUsage 都带
// output_tokens_details.thinking_tokens（据 anthropic-sdk-typescript master：
// OutputTokensDetails.thinking_tokens，注释明确「output_tokens 仍是含推理在内的
// 权威计费总量，本对象只是只读的可观测分解」）。此前 wireUsage 未建模它→
// anthropic 解码不填 IR.Usage.ReasoningTokens（chat 从 completion_tokens_details.
// reasoning_tokens、responses 从 output_tokens_details.reasoning_tokens 都填了，
// 唯 anthropic 漏），推理占比这一成本归因维度被 json.Unmarshal 静默吞掉；编码回
// anthropic 客户端时也写不出。这组测试把 thinking_tokens 钉死在解码（非流式/
// message_start/message_delta）与编码（非流式/message_start/message_delta）双向，
// 并确认 ReasoningTokens==0 时不伪造空分解对象。

// usageThinking 从 usage JSON 里取 output_tokens_details.thinking_tokens；
// 没有该对象返回 -1（区别于「有对象、值为 0」）。
func usageThinking(t *testing.T, raw json.RawMessage) int64 {
	t.Helper()
	var u map[string]json.RawMessage
	if err := json.Unmarshal(raw, &u); err != nil {
		t.Fatalf("usage 解不开：%v", err)
	}
	d, ok := u["output_tokens_details"]
	if !ok {
		return -1
	}
	var det struct {
		ThinkingTokens int64 `json:"thinking_tokens"`
	}
	if err := json.Unmarshal(d, &det); err != nil {
		t.Fatalf("output_tokens_details 解不开：%v", err)
	}
	return det.ThinkingTokens
}

// 解码：非流式响应从 usage.output_tokens_details.thinking_tokens 读出推理分解。
func TestReasoningTokensDecodedNonStreaming(t *testing.T) {
	body := []byte(`{"id":"m1","type":"message","role":"assistant","model":"m",` +
		`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":10,"output_tokens":8,` +
		`"output_tokens_details":{"thinking_tokens":5}}}`)
	resp, err := DecodeResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.ReasoningTokens != 5 {
		t.Fatalf("thinking_tokens 没读进 IR.Usage.ReasoningTokens，得到 %d，想要 5",
			resp.Usage.ReasoningTokens)
	}
	// 总量不受影响：output_tokens 仍是权威计费值。
	if resp.Usage.OutputTokens != 8 {
		t.Errorf("output_tokens 被改动，得到 %d，想要 8", resp.Usage.OutputTokens)
	}
}

// 解码：流式 message_start 从 message.usage.output_tokens_details 读出。
func TestReasoningTokensDecodedStreamingMessageStart(t *testing.T) {
	d := newStreamDecoder()
	evs, err := d.Feed("message_start",
		`{"type":"message_start","message":{"id":"m1","model":"m","role":"assistant",`+
			`"content":[],"usage":{"input_tokens":10,"output_tokens":0,`+
			`"output_tokens_details":{"thinking_tokens":3}}}}`)
	if err != nil {
		t.Fatal(err)
	}
	var got int64 = -1
	for _, ev := range evs {
		if ev.Type == ir.EvMessageStart && ev.Usage != nil {
			got = ev.Usage.ReasoningTokens
		}
	}
	if got != 3 {
		t.Fatalf("message_start 的 thinking_tokens 没读进事件，得到 %d，想要 3", got)
	}
}

// 解码：流式 message_delta 的精简 usage 也带 output_tokens_details（官方
// MessageDeltaUsage 有该键），按 wireUsage 宽松读同样能取出。
func TestReasoningTokensDecodedStreamingMessageDelta(t *testing.T) {
	d := newStreamDecoder()
	evs, err := d.Feed("message_delta",
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},`+
			`"usage":{"output_tokens":8,"output_tokens_details":{"thinking_tokens":5}}}`)
	if err != nil {
		t.Fatal(err)
	}
	var got int64 = -1
	for _, ev := range evs {
		if ev.Type == ir.EvMessageDelta && ev.Usage != nil {
			got = ev.Usage.ReasoningTokens
		}
	}
	if got != 5 {
		t.Fatalf("message_delta 的 thinking_tokens 没读进事件，得到 %d，想要 5", got)
	}
}

// 编码：非流式响应把 IR.Usage.ReasoningTokens 写进 usage.output_tokens_details。
func TestReasoningTokensEncodedNonStreaming(t *testing.T) {
	out, err := EncodeResponse(&ir.Response{
		ID: "m1", Model: "m",
		Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}},
		Usage:   ir.Usage{InputTokens: 10, OutputTokens: 8, ReasoningTokens: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatal(err)
	}
	u, ok := top["usage"]
	if !ok {
		t.Fatalf("响应缺 usage：%s", out)
	}
	if got := usageThinking(t, u); got != 5 {
		t.Errorf("usage.output_tokens_details.thinking_tokens 没写出，得到 %d，想要 5：%s", got, out)
	}
}

// 编码：ReasoningTokens==0 时不写 output_tokens_details（不伪造全零分解对象，
// 与 cache_creation 仅在明细已知时写出同款纪律）。
func TestReasoningTokensZeroOmitsDetailsNonStreaming(t *testing.T) {
	out, err := EncodeResponse(&ir.Response{
		ID: "m1", Model: "m",
		Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}},
		Usage:   ir.Usage{InputTokens: 10, OutputTokens: 8},
	})
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatal(err)
	}
	var u map[string]json.RawMessage
	if err := json.Unmarshal(top["usage"], &u); err != nil {
		t.Fatal(err)
	}
	if _, ok := u["output_tokens_details"]; ok {
		t.Errorf("ReasoningTokens==0 不该写出 output_tokens_details：%s", out)
	}
}

// 编码：流式 message_start 把推理分解写进 message.usage.output_tokens_details。
func TestReasoningTokensEncodedStreamingMessageStart(t *testing.T) {
	e := newStreamEncoder()
	frames, err := e.Encode(ir.Event{
		Type: ir.EvMessageStart, MessageID: "m1", Model: "m",
		Usage: &ir.Usage{InputTokens: 10, OutputTokens: 0, ReasoningTokens: 3},
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
	u, ok := msg["usage"]
	if !ok {
		t.Fatalf("message_start 缺 usage：%s", frames[0])
	}
	if got := usageThinking(t, u); got != 3 {
		t.Errorf("message.usage.output_tokens_details.thinking_tokens 没写出，得到 %d，想要 3：%s",
			got, frames[0])
	}
}

// 编码：流式 message_delta 的精简 usage 也写出 output_tokens_details
// （官方 MessageDeltaUsage 有该键，同族非流式→流式聚合路径靠它带回推理分解）。
func TestReasoningTokensEncodedStreamingMessageDelta(t *testing.T) {
	e := newStreamEncoder()
	frames, err := e.Encode(ir.Event{
		Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn,
		Usage: &ir.Usage{OutputTokens: 8, ReasoningTokens: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) == 0 {
		t.Fatal("没有产出 message_delta 帧")
	}
	// 流式编码器会先补一帧 message_start 开流，message_delta 在其后：按 type 定位。
	var u json.RawMessage
	for _, f := range frames {
		top := dataOf(t, f)
		var typ string
		_ = json.Unmarshal(top["type"], &typ)
		if typ != "message_delta" {
			continue
		}
		var ok bool
		if u, ok = top["usage"]; !ok {
			t.Fatalf("message_delta 帧缺 usage：%s", f)
		}
	}
	if u == nil {
		t.Fatalf("没有产出 message_delta 帧：%q", frames)
	}
	if got := usageThinking(t, u); got != 5 {
		t.Errorf("message_delta 的 output_tokens_details.thinking_tokens 没写出，得到 %d，想要 5", got)
	}
}

// 同族往返：解码出的推理分解经编码原样回到线上，数值不漂。
func TestReasoningTokensRoundTrip(t *testing.T) {
	body := []byte(`{"id":"m1","type":"message","role":"assistant","model":"m",` +
		`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":10,"output_tokens":8,` +
		`"output_tokens_details":{"thinking_tokens":5}}}`)
	resp, err := DecodeResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatal(err)
	}
	if got := usageThinking(t, top["usage"]); got != 5 {
		t.Errorf("往返后 thinking_tokens 漂了，得到 %d，想要 5：%s", got, out)
	}
}
