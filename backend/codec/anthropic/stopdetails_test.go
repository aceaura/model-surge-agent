package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// #70 的 anthropic 响应侧结构回执：stop_details（拒绝分类）、
// usage.server_tool_use（托管工具执行次数）、usage.inference_geo（实际推理区域）。
// 三位都是双向：同族往返丢任何一位，客户端拿到的回执就比上游少一截，
// 而两侧都不报错。

// 流式：stop_details 随 message_delta 到达；usage 的三位新回执同帧收下。
func TestStreamDeltaCarriesStopDetailsAndUsageReceipts(t *testing.T) {
	raw := `event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"refusal","stop_details":{"type":"refusal","category":"refusal_other","explanation":"policy"}},"usage":{"output_tokens":10,"server_tool_use":{"web_search_requests":2,"web_fetch_requests":1},"inference_geo":"us"}}

event: message_stop
data: {"type":"message_stop"}

`
	events := feed(t, raw)
	var delta *ir.Event
	for i := range events {
		if events[i].Type == ir.EvMessageDelta {
			delta = &events[i]
		}
	}
	if delta == nil {
		t.Fatalf("没有 delta 事件：%+v", events)
	}
	if delta.StopDetails == nil || delta.StopDetails.Category != "refusal_other" ||
		delta.StopDetails.Explanation != "policy" {
		t.Errorf("stop_details 没解出来：%+v", delta.StopDetails)
	}
	if delta.Usage == nil {
		t.Fatalf("delta 没带 usage")
	}
	if delta.Usage.WebSearchRequests != 2 || delta.Usage.WebFetchRequests != 1 {
		t.Errorf("server_tool_use 丢了：%+v", delta.Usage)
	}
	if delta.Usage.InferenceGeo != "us" {
		t.Errorf("inference_geo 丢了：%q", delta.Usage.InferenceGeo)
	}
}

// 官方文档写明 category/explanation 可显式 null（≡ 缺省）。显式 null 不许
// 把整个 stop_details 解炸，也不许读成非零字符串。
func TestStopDetailsExplicitNullDecodesToEmpty(t *testing.T) {
	raw := `event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"refusal","stop_details":{"type":"refusal","category":null,"explanation":null}}}

`
	events := feed(t, raw)
	for _, ev := range events {
		if ev.Type == ir.EvMessageDelta && ev.StopDetails != nil {
			if ev.StopDetails.Category != "" || ev.StopDetails.Explanation != "" {
				t.Errorf("显式 null 读出了值：%+v", ev.StopDetails)
			}
			return
		}
	}
	t.Fatalf("显式 null 的 stop_details 整个丢了：%+v", events)
}

// 非流式：整份响应的 stop_details 与 usage 三位回执。
func TestWholeResponseCarriesStopDetailsAndUsageReceipts(t *testing.T) {
	body := []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
		`"content":[{"type":"text","text":"hi"}],"stop_reason":"refusal",` +
		`"stop_details":{"type":"refusal","category":"c1","explanation":"e1"},` +
		`"usage":{"input_tokens":5,"output_tokens":7,` +
		`"server_tool_use":{"web_search_requests":3,"web_fetch_requests":4},"inference_geo":"eu"}}`)
	resp, err := DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.StopDetails == nil || resp.StopDetails.Category != "c1" ||
		resp.StopDetails.Explanation != "e1" {
		t.Errorf("stop_details 没解出来：%+v", resp.StopDetails)
	}
	if resp.Usage.WebSearchRequests != 3 || resp.Usage.WebFetchRequests != 4 {
		t.Errorf("server_tool_use 丢了：%+v", resp.Usage)
	}
	if resp.Usage.InferenceGeo != "eu" {
		t.Errorf("inference_geo 丢了：%q", resp.Usage.InferenceGeo)
	}

	// 同族回吐：三位都要原样写出。
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	for _, want := range []string{
		`"stop_details":{"type":"refusal","category":"c1","explanation":"e1"}`,
		`"server_tool_use":{"web_search_requests":3,"web_fetch_requests":4}`,
		`"inference_geo":"eu"`,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("出站缺 %s：%s", want, out)
		}
	}
}

// 回执全零时不凭空造键：server_tool_use / inference_geo / stop_details
// 都必须缺席，否则下游会把「没跑托管工具」读成「跑了零次」。
func TestZeroReceiptsStayAbsent(t *testing.T) {
	out, err := EncodeResponse(&ir.Response{ID: "msg_1", Model: "m",
		StopReason: ir.StopEndTurn, Usage: ir.Usage{InputTokens: 1, OutputTokens: 2}})
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	for _, banned := range []string{"server_tool_use", "inference_geo", "stop_details"} {
		if strings.Contains(string(out), banned) {
			t.Errorf("零值回执被凭空写出 %s：%s", banned, out)
		}
	}
}

// stop_details 只给了 type（category/explanation 为空）时，回写不带空串键：
// 官方形态里两位可缺席，写出 "" 与 null 都不是上游的口径。
func TestStopDetailsEmptyFieldsOmittedOnEncode(t *testing.T) {
	out, err := EncodeResponse(&ir.Response{ID: "msg_1", Model: "m",
		StopReason: ir.StopReason("refusal"), StopDetails: &ir.StopDetails{}})
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if !strings.Contains(string(out), `"stop_details":{"type":"refusal"}`) {
		t.Errorf("空的 category/explanation 没按缺席处理：%s", out)
	}
}

// 流式出站：delta 事件上的 stop_details 要写进 message_delta 帧。
func TestStreamEncoderEmitsStopDetails(t *testing.T) {
	enc := newStreamEncoder()
	var frames [][]byte
	out, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "msg_1", Model: "m"})
	if err != nil {
		t.Fatalf("Encode(start): %v", err)
	}
	frames = append(frames, out...)
	out, err = enc.Encode(ir.Event{Type: ir.EvMessageDelta,
		StopReason: ir.StopReason("refusal"),
		StopDetails: &ir.StopDetails{
			Category: "c2", Explanation: "e2"}})
	if err != nil {
		t.Fatalf("Encode(delta): %v", err)
	}
	frames = append(frames, out...)
	frames = append(frames, enc.Finish()...)
	var joined []byte
	for _, f := range frames {
		joined = append(joined, f...)
	}
	if !strings.Contains(string(joined),
		`"stop_details":{"type":"refusal","category":"c2","explanation":"e2"}`) {
		t.Errorf("流式出站丢了 stop_details：%s", joined)
	}
}
