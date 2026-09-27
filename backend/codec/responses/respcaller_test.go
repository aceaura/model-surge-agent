package responses

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次37：responses function_call/custom_tool_call 条目上的 caller（发起方标记，
// union direct{caller_id}|program）、namespace（命名空间）、async（异步标记）是
// responses 一族独有的纯 provenance。此前 wireItem/wireRespItem 都不建模这三键，
// json.Unmarshal 静默丢弃，responses→responses 同族往返一次即蒸发。这组测试钉住
// 三条解码路径（请求历史、非流式响应、流式）读出、两条编码路径（请求、响应/流式）
// 逐字带回，以及缺席时不凭空造键。

// 请求历史里的 caller/namespace/async 落 IR，同族重编码原样带回。
func TestRespCallerRequestRoundTrip(t *testing.T) {
	body := `{"model":"gpt-x","input":[` +
		`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get","arguments":"{}",` +
		`"caller":{"type":"program"},"namespace":"ns_a","async":true},` +
		`{"type":"custom_tool_call","id":"ctc_1","call_id":"call_2","name":"free","input":"raw",` +
		`"caller":{"type":"direct","caller_id":"c9"},"namespace":"ns_b","async":false}` +
		`]}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	var fc, ctc *ir.ToolUse
	for _, m := range req.Messages {
		for i := range m.Content {
			b := &m.Content[i]
			if b.Type != ir.BlockToolUse || b.ToolUse == nil {
				continue
			}
			if b.ToolUse.Kind == ir.ToolCustom {
				ctc = b.ToolUse
			} else {
				fc = b.ToolUse
			}
		}
	}
	if fc == nil || ctc == nil {
		t.Fatalf("两条调用未都解出: fc=%v ctc=%v", fc, ctc)
	}
	if string(fc.ResponsesCaller) != `{"type":"program"}` || fc.ResponsesNamespace != "ns_a" ||
		fc.ResponsesAsync == nil || !*fc.ResponsesAsync {
		t.Errorf("function_call provenance 解码即丢: caller=%q ns=%q async=%v",
			fc.ResponsesCaller, fc.ResponsesNamespace, fc.ResponsesAsync)
	}
	// async:false 必须与「缺席」区分——解码成 nil 就改了语义。
	if string(ctc.ResponsesCaller) != `{"type":"direct","caller_id":"c9"}` || ctc.ResponsesNamespace != "ns_b" ||
		ctc.ResponsesAsync == nil || *ctc.ResponsesAsync {
		t.Errorf("custom_tool_call provenance 解码即丢: caller=%q ns=%q async=%v",
			ctc.ResponsesCaller, ctc.ResponsesNamespace, ctc.ResponsesAsync)
	}

	out, err := EncodeRequest(req.Clone())
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		`"caller":{"type":"program"}`, `"namespace":"ns_a"`, `"async":true`,
		`"caller":{"type":"direct","caller_id":"c9"}`, `"namespace":"ns_b"`, `"async":false`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("重编码丢了 %s: %s", want, s)
		}
	}

	// 对照组：缺席 provenance 的调用不得凭空造 caller/namespace/async 键。
	plain, err := DecodeRequest([]byte(
		`{"model":"gpt-x","input":[{"type":"function_call","call_id":"c","name":"g","arguments":"{}"}]}`))
	if err != nil {
		t.Fatalf("DecodeRequest(plain): %v", err)
	}
	pout, err := EncodeRequest(plain)
	if err != nil {
		t.Fatalf("EncodeRequest(plain): %v", err)
	}
	for _, bad := range []string{`"caller"`, `"namespace"`, `"async"`} {
		if strings.Contains(string(pout), bad) {
			t.Errorf("缺席的 provenance 被发明 %s: %s", bad, pout)
		}
	}
}

// 非流式响应解码读出 provenance，响应编码逐字带回。
func TestRespCallerResponseRoundTrip(t *testing.T) {
	resp, _, err := DecodeResponseLossy([]byte(
		`{"id":"resp_1","model":"gpt-x","status":"completed","output":[` +
			`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get","arguments":"{}",` +
			`"caller":{"type":"program"},"namespace":"ns_r","async":true}` +
			`]}`))
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	var tu *ir.ToolUse
	for i := range resp.Content {
		b := &resp.Content[i]
		if b.Type == ir.BlockToolUse && b.ToolUse != nil {
			tu = b.ToolUse
		}
	}
	if tu == nil {
		t.Fatalf("响应里的 function_call 未解出")
	}
	if string(tu.ResponsesCaller) != `{"type":"program"}` || tu.ResponsesNamespace != "ns_r" ||
		tu.ResponsesAsync == nil || !*tu.ResponsesAsync {
		t.Fatalf("响应侧 provenance 解码即丢: caller=%q ns=%q async=%v",
			tu.ResponsesCaller, tu.ResponsesNamespace, tu.ResponsesAsync)
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	s := string(out)
	for _, want := range []string{`"caller":{"type":"program"}`, `"namespace":"ns_r"`, `"async":true`} {
		if !strings.Contains(s, want) {
			t.Errorf("响应重编码丢了 %s: %s", want, s)
		}
	}
}

// 流式：item.added 帧携带 provenance，解码落 EvBlockStart 块；编码侧带块开条目，
// added/done 帧逐字带回。
func TestRespCallerStreamRoundTrip(t *testing.T) {
	dec := newStreamDecoder()
	var events []ir.Event
	for _, raw := range []string{
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-x"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_real","call_id":"call_1","name":"get","arguments":"","caller":{"type":"program"},"namespace":"ns_s","async":true}}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{}"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_real","call_id":"call_1","name":"get","arguments":"{}"}}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`,
	} {
		evs, err := dec.Feed("", raw)
		if err != nil {
			t.Fatalf("Feed(%s): %v", raw, err)
		}
		events = append(events, evs...)
	}
	var tu *ir.ToolUse
	for _, ev := range events {
		if ev.Type == ir.EvBlockStart && ev.Block != nil &&
			ev.Block.Type == ir.BlockToolUse && ev.Block.ToolUse != nil {
			tu = ev.Block.ToolUse
		}
	}
	if tu == nil {
		t.Fatalf("流式解码未产出 tool_use 块")
	}
	if string(tu.ResponsesCaller) != `{"type":"program"}` || tu.ResponsesNamespace != "ns_s" ||
		tu.ResponsesAsync == nil || !*tu.ResponsesAsync {
		t.Fatalf("流式解码丢了 provenance: caller=%q ns=%q async=%v",
			tu.ResponsesCaller, tu.ResponsesNamespace, tu.ResponsesAsync)
	}

	// 编码侧：带着 provenance 开块，added/done 帧必须逐字带回。
	enc := newStreamEncoder()
	var wire strings.Builder
	feed := func(ev ir.Event) {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("encode %v: %v", ev.Type, err)
		}
		for _, f := range frames {
			wire.Write(f)
		}
	}
	asyncTrue := true
	feed(ir.Event{Type: ir.EvMessageStart, MessageID: "resp_1", Model: "gpt-x"})
	feed(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
		Type: ir.BlockToolUse,
		ToolUse: &ir.ToolUse{ID: "call_1", Name: "get", ItemID: "fc_real",
			ResponsesCaller:    []byte(`{"type":"program"}`),
			ResponsesNamespace: "ns_s",
			ResponsesAsync:     &asyncTrue}}})
	feed(ir.Event{Type: ir.EvToolInput, Index: 0, Text: "{}"})
	feed(ir.Event{Type: ir.EvBlockStop, Index: 0})
	feed(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopToolUse})
	feed(ir.Event{Type: ir.EvMessageStop})
	for _, f := range enc.Finish() {
		wire.Write(f)
	}
	s := wire.String()
	for _, want := range []string{`"caller":{"type":"program"}`, `"namespace":"ns_s"`, `"async":true`} {
		if !strings.Contains(s, want) {
			t.Errorf("流式编码没携带 %s:\n%s", want, s)
		}
	}
}
