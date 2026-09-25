package responses

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 同族往返保真三件事：
//   - item id（msg_…/rs_…/fc_…/ctc_…）随 IR 三槽位往返：store=true 时上游
//     存的条目按 item id 索引，换成合成 id 后 item_reference 全部错指；
//   - reasoning 条目的 content 数组（reasoning_text part）在 summary 为空时
//     兜底，正文不再整条丢；
//   - response.reasoning_summary_part.added 是官方 part 边界标记，计进度帧
//     账，不得混进「解码器不认识」的未知账。

// 请求历史的 item id 落 IR 三槽位，同族重编码原样带回；没给 id 的条目
// 不得发明 id 键。
func TestItemIDRoundTrip(t *testing.T) {
	body := `{"model":"gpt-x","input":[` +
		`{"type":"message","id":"msg_aaa","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
		`{"type":"message","id":"msg_bbb","role":"assistant","content":[{"type":"output_text","text":"ok"}]},` +
		`{"type":"reasoning","id":"rs_ccc","summary":[{"type":"summary_text","text":"thought"}],"encrypted_content":"enc"},` +
		`{"type":"function_call","id":"fc_ddd","call_id":"call_1","name":"get","arguments":"{}"},` +
		`{"type":"custom_tool_call","id":"ctc_eee","call_id":"call_2","name":"free","input":"raw text"}` +
		`]}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	var msgUser, msgAsst, rsID, fcID, ctcID string
	for _, m := range req.Messages {
		if m.ItemID != "" {
			if m.Role == ir.RoleUser {
				msgUser = m.ItemID
			} else {
				msgAsst = m.ItemID
			}
		}
		for _, b := range m.Content {
			if b.Type == ir.BlockThinking && b.Thinking != nil {
				rsID = b.Thinking.ItemID
			}
			if b.Type == ir.BlockToolUse && b.ToolUse != nil {
				if b.ToolUse.Kind == ir.ToolCustom {
					ctcID = b.ToolUse.ItemID
				} else {
					fcID = b.ToolUse.ItemID
				}
			}
		}
	}
	if msgUser != "msg_aaa" || msgAsst != "msg_bbb" || rsID != "rs_ccc" ||
		fcID != "fc_ddd" || ctcID != "ctc_eee" {
		t.Fatalf("item id 解码即丢: msg=%q/%q rs=%q fc=%q ctc=%q",
			msgUser, msgAsst, rsID, fcID, ctcID)
	}

	out, err := EncodeRequest(req.Clone())
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		`"id":"msg_aaa"`, `"id":"msg_bbb"`, `"id":"rs_ccc"`, `"id":"fc_ddd"`, `"id":"ctc_eee"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("重编码丢了 %s: %s", want, s)
		}
	}

	// 对照组：没给 id 的条目不得发明 id 键（非流式编码不合成）。
	plain, err := DecodeRequest([]byte(
		`{"model":"gpt-x","input":[{"type":"function_call","call_id":"call_1","name":"get","arguments":"{}"}]}`))
	if err != nil {
		t.Fatalf("DecodeRequest(plain): %v", err)
	}
	pout, err := EncodeRequest(plain)
	if err != nil {
		t.Fatalf("EncodeRequest(plain): %v", err)
	}
	if strings.Contains(string(pout), `"id":"fc_`) {
		t.Errorf("缺席的 item id 被发明: %s", pout)
	}
}

// 流式解码把 item id 落进 EvBlockStart 的块；流式编码优先携带原号，
// 不携带时也不合成假号。
func TestStreamItemIDRoundTrip(t *testing.T) {
	dec := newStreamDecoder()
	var events []ir.Event
	for _, raw := range []string{
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-x"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_real","call_id":"call_1","name":"get","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{}"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_real","call_id":"call_1","name":"get","arguments":"{}"}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"reasoning","id":"rs_real","summary":[]}}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"reasoning","id":"rs_real","summary":[],"encrypted_content":"enc"}}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`,
	} {
		evs, err := dec.Feed("", raw)
		if err != nil {
			t.Fatalf("Feed(%s): %v", raw, err)
		}
		events = append(events, evs...)
	}
	var fcID, rsID string
	for _, ev := range events {
		if ev.Type != ir.EvBlockStart || ev.Block == nil {
			continue
		}
		if ev.Block.Type == ir.BlockToolUse && ev.Block.ToolUse != nil {
			fcID = ev.Block.ToolUse.ItemID
		}
		if ev.Block.Type == ir.BlockThinking && ev.Block.Thinking != nil {
			rsID = ev.Block.Thinking.ItemID
		}
	}
	if fcID != "fc_real" || rsID != "rs_real" {
		t.Fatalf("流式解码丢了 item id: fc=%q rs=%q", fcID, rsID)
	}

	// 编码侧：带着 ItemID 开块，added/done 帧必须用原号。
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
	feed(ir.Event{Type: ir.EvMessageStart, MessageID: "resp_1", Model: "gpt-x"})
	feed(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
		Type:    ir.BlockToolUse,
		ToolUse: &ir.ToolUse{ID: "call_1", Name: "get", ItemID: "fc_real"}}})
	feed(ir.Event{Type: ir.EvToolInput, Index: 0, Text: "{}"})
	feed(ir.Event{Type: ir.EvBlockStop, Index: 0})
	feed(ir.Event{Type: ir.EvBlockStart, Index: 1, Block: &ir.Block{
		Type: ir.BlockThinking,
		Thinking: &ir.Thinking{Text: "t", Signature: "enc",
			SignatureFrom: Name, ItemID: "rs_real"}}})
	feed(ir.Event{Type: ir.EvBlockStop, Index: 1})
	feed(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopToolUse})
	feed(ir.Event{Type: ir.EvMessageStop})
	for _, f := range enc.Finish() {
		wire.Write(f)
	}
	s := wire.String()
	if !strings.Contains(s, `"id":"fc_real"`) || !strings.Contains(s, `"id":"rs_real"`) {
		t.Errorf("流式编码没携带原 item id:\n%s", s)
	}
	// 本仓流式编码从不合成 item id：缺席就是缺席，不得凭空造号。
	if strings.Contains(s, `"id":"fc_0`) || strings.Contains(s, `"id":"rs_0`) {
		t.Errorf("流式编码合成了假 item id:\n%s", s)
	}
}

// reasoning 条目 summary 为空时正文走 content 数组（reasoning_text part）
// 兜底：请求解码、流式 done 帧、非流式响应三条路径同判据。
func TestReasoningContentFallback(t *testing.T) {
	// 请求历史。
	body := `{"model":"gpt-x","input":[{"type":"reasoning","id":"rs_1","summary":[],` +
		`"content":[{"type":"reasoning_text","text":"deep thought"}]}]}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	var text string
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockThinking && b.Thinking != nil {
				text = b.Thinking.Text
			}
		}
	}
	if text != "deep thought" {
		t.Fatalf("请求侧 reasoning content 兜底失效: %q", text)
	}

	// 流式 done 帧：summary 空、content 有正文。
	resp, _ := feedEvents(t,
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-x"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[],"content":[{"type":"reasoning_text","text":"stream thought"}]}}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`,
	)
	var streamText string
	for _, b := range resp.Content {
		if b.Type == ir.BlockThinking && b.Thinking != nil {
			streamText += b.Thinking.Text
		}
	}
	if streamText != "stream thought" {
		t.Errorf("流式 done 帧 reasoning content 兜底失效: %q", streamText)
	}

	// 非流式响应解码。
	ns, _, err := DecodeResponseLossy([]byte(
		`{"id":"resp_1","model":"gpt-x","status":"completed","output":[` +
			`{"type":"reasoning","id":"rs_1","summary":[],"content":[{"type":"reasoning_text","text":"done thought"}]}` +
			`]}`))
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	var nsText string
	for _, b := range ns.Content {
		if b.Type == ir.BlockThinking && b.Thinking != nil {
			nsText += b.Thinking.Text
		}
	}
	if nsText != "done thought" {
		t.Errorf("非流式响应 reasoning content 兜底失效: %q", nsText)
	}
}

// response.reasoning_summary_part.added 是官方 part 边界标记（正文随
// summary_text.done 回补）：计进度帧账，不得混进未知帧账。
func TestReasoningSummaryPartAddedCountedAsProgress(t *testing.T) {
	dec := newStreamDecoder()
	evs, err := dec.Feed("",
		`{"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(evs) != 0 {
		t.Errorf("边界标记帧不该产出 IR 事件：%+v", evs)
	}
	notes := dec.Notes()
	if len(notes) != 1 || !strings.Contains(notes[0], "progress frame(s)") {
		t.Fatalf("官方事件被计错账：%q", notes)
	}
	if strings.Contains(notes[0], "does not know") {
		t.Errorf("官方事件被报成未知型：%q", notes[0])
	}
}
