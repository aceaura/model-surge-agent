package responses

import (
	"encoding/json"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 思考块开块补发 reasoning_summary_part.added：Codex 类严格客户端只在
// part.added 之后渲染 summary delta，缺这帧整条思考摘要在客户端不可见
// （sub2api 与 cc-switch 的 responses 桥都显式合成这一帧）。
func TestThinkingBlockStartEmitsSummaryPartAdded(t *testing.T) {
	enc := newStreamEncoder()
	frames, err := enc.Encode(ir.Event{Type: ir.EvBlockStart, Index: 0,
		Block: &ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{}}})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("开块应发 output_item.added + summary_part.added 两帧，实发 %d", len(frames))
	}
	kind, second := parseFrame(t, frames[1])
	if kind != evReasoningSummaryPartAdded {
		t.Fatalf("第二帧类型 = %q", kind)
	}
	// summary_index 是零值也必须写出（requiredIndexFields 补写）：
	// 缺键的 part.added 在严格客户端上等于没发。
	var si int
	if raw, ok := second["summary_index"]; !ok || json.Unmarshal(raw, &si) != nil || si != 0 {
		t.Fatalf("summary_index 应写出且为 0：%v", second["summary_index"])
	}
	var part struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(second["part"], &part); err != nil || part.Type != partSummaryText {
		t.Fatalf("part.type = %+v, err = %v", part, err)
	}
	if _, ok := second["sequence_number"]; !ok {
		t.Fatal("合成帧也要过 frame 漏斗带 sequence_number")
	}
}

// incomplete_details.reason=max_messages 是独立停止档：与 max_output_tokens
// 两回事（消息数上限 vs 输出长度上限），解码不并档，编码原值回写。
func TestMaxMessagesIncompleteRoundTrip(t *testing.T) {
	d := newStreamDecoder()
	if _, err := d.Feed("", `{"type":"response.created","response":{"id":"r1","model":"gpt"}}`); err != nil {
		t.Fatal(err)
	}
	evs, err := d.Feed("",
		`{"type":"response.incomplete","response":{"id":"r1","status":"incomplete",`+
			`"incomplete_details":{"reason":"max_messages"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	var got ir.StopReason
	for _, ev := range evs {
		if ev.Type == ir.EvMessageDelta {
			got = ev.StopReason
		}
	}
	if got != ir.StopMaxMessages {
		t.Fatalf("max_messages 应成独立档：%q", got)
	}
	// 回写原值，不塌成 max_output_tokens。
	status, incomplete := renderStatus(ir.StopMaxMessages)
	if status != "incomplete" || incomplete == nil || incomplete.Reason != "max_messages" {
		t.Fatalf("renderStatus = %q, %+v", status, incomplete)
	}
	// max_output_tokens 与未识别 reason 的既有口径不动。
	if s, i := renderStatus(ir.StopMaxTokens); s != "incomplete" || i.Reason != "max_output_tokens" {
		t.Fatalf("StopMaxTokens 回写变形：%q %+v", s, i)
	}
}

// 多段 reasoning summary 合并计数：summary_index>0 的帧正文进 IR 但 part
// 边界变形，必须报得出；只有 index 0 的正常流闭嘴。
func TestMergedSummaryNote(t *testing.T) {
	d := newStreamDecoder()
	for _, raw := range []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`,
		`{"type":"response.reasoning_summary_part.added","output_index":0,"item_id":"rs_1","summary_index":0,"part":{"type":"summary_text","text":""}}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"item_id":"rs_1","summary_index":0,"delta":"第一段"}`,
		// summary_index>0 的 delta、text.done、part.done 各计一次。
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"item_id":"rs_1","summary_index":1,"delta":"第二段"}`,
		`{"type":"response.reasoning_summary_text.done","output_index":0,"item_id":"rs_1","summary_index":1,"text":"第二段"}`,
		`{"type":"response.reasoning_summary_part.done","output_index":0,"item_id":"rs_1","summary_index":1,"part":{"type":"summary_text","text":"第二段"}}`,
	} {
		if _, err := d.Feed("", raw); err != nil {
			t.Fatalf("Feed(%s): %v", raw, err)
		}
	}
	if !anyNoteHas(d.Notes(), "merged 3 reasoning summary frame(s)") {
		t.Fatalf("合并注记缺失：%q", d.Notes())
	}
	// 排干后不重复报。
	if anyNoteHas(d.Notes(), "merged") {
		t.Fatal("Notes() 未排干")
	}

	quiet := newStreamDecoder()
	for _, raw := range []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"item_id":"rs_1","summary_index":0,"delta":"只有一段"}`,
	} {
		if _, err := quiet.Feed("", raw); err != nil {
			t.Fatalf("Feed(%s): %v", raw, err)
		}
	}
	if anyNoteHas(quiet.Notes(), "merged") {
		t.Fatalf("单段 summary 误报：%q", quiet.Notes())
	}
}
