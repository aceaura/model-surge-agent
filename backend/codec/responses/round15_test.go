package responses

import (
	"strings"
	"testing"
)

// 轮次15 B-B：流式路径丢失 reasoning 通道标记的有损注记。
//
// 官方 reasoning item 有两条独立通道——summary（reasoning_summary_text，给
// 用户看的摘要）与 content（reasoning_text，模型内部推理原文）。非流式路径用
// Thinking.ContentChannel 标记并同族原样回吐（见 r110_test）。但流式的事件模型
// 没有这个槽位：增量帧不带通道标记，编码端一律渲染成 summary，于是同族流式
// 往返把 content 通道的推理塌缩进 summary——正文保留、通道语义丢失。这属
// 「不得静默丢弃」不变量覆盖的有损改写，必须在 Notes() 里报出。
//
// 计数按块去重：同一 reasoning 块的 delta/done/itemDone 多帧只计一次。

const contentChannelNote = "content-channel reasoning block"

// content 通道增量帧触发注记，计数为 1。
func TestR15ContentChannelDeltaNoted(t *testing.T) {
	_, notes := feedEvents(t,
		`{"type":"response.created","response":{"id":"r1","model":"m"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}`,
		`{"type":"response.reasoning_text.delta","output_index":0,"delta":"INNER"}`,
		`{"type":"response.reasoning_text.done","output_index":0,"text":"INNER"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}`,
		`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed"}}`,
	)
	if !anyNoteHas(notes, "1 "+contentChannelNote) {
		t.Errorf("content 通道增量没按数报出：%v", notes)
	}
}

// 只有 done 帧（done-only 上游）也触发注记。
func TestR15ContentChannelDoneOnlyNoted(t *testing.T) {
	_, notes := feedEvents(t,
		`{"type":"response.created","response":{"id":"r1","model":"m"}}`,
		`{"type":"response.reasoning_text.done","output_index":0,"text":"INNER"}`,
		`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed"}}`,
	)
	if !anyNoteHas(notes, "1 "+contentChannelNote) {
		t.Errorf("content 通道 done 帧没报出：%v", notes)
	}
}

// itemDone 兜底路径（summary 空、正文在 content 数组）触发注记。
func TestR15ContentChannelItemDoneFallbackNoted(t *testing.T) {
	_, notes := feedEvents(t,
		`{"type":"response.created","response":{"id":"r1","model":"m"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[],"content":[{"type":"reasoning_text","text":"INNER"}]}}`,
		`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed"}}`,
	)
	if !anyNoteHas(notes, "1 "+contentChannelNote) {
		t.Errorf("itemDone content 通道兜底没报出：%v", notes)
	}
}

// summary 通道（默认）不许误报：维持历史行为，没有通道塌缩就没有注记。
func TestR15SummaryChannelProducesNoNote(t *testing.T) {
	_, notes := feedEvents(t,
		`{"type":"response.created","response":{"id":"r1","model":"m"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"SUM"}`,
		`{"type":"response.reasoning_summary_text.done","output_index":0,"summary_index":0,"text":"SUM"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"SUM"}]}}`,
		`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed"}}`,
	)
	if anyNoteHas(notes, contentChannelNote) {
		t.Errorf("summary 通道被误报成 content 通道塌缩：%v", notes)
	}
}

// 多个 content 通道块分别计数。
func TestR15MultipleContentChannelBlocksCounted(t *testing.T) {
	_, notes := feedEvents(t,
		`{"type":"response.created","response":{"id":"r1","model":"m"}}`,
		`{"type":"response.reasoning_text.delta","output_index":0,"delta":"A"}`,
		`{"type":"response.reasoning_text.delta","output_index":1,"delta":"B"}`,
		`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed"}}`,
	)
	if !anyNoteHas(notes, "2 "+contentChannelNote) {
		t.Errorf("两个 content 通道块没按数报出：%v", notes)
	}
}

// 同一块的 delta + done + itemDone 三帧只计一次（去重）。
func TestR15SameBlockDedupedToOne(t *testing.T) {
	_, notes := feedEvents(t,
		`{"type":"response.created","response":{"id":"r1","model":"m"}}`,
		`{"type":"response.reasoning_text.delta","output_index":0,"delta":"IN"}`,
		`{"type":"response.reasoning_text.done","output_index":0,"text":"INNER"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[],"content":[{"type":"reasoning_text","text":"INNER"}]}}`,
		`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed"}}`,
	)
	if !anyNoteHas(notes, "1 "+contentChannelNote) {
		t.Errorf("同一块多帧没去重成 1：%v", notes)
	}
	if anyNoteHas(notes, "3 "+contentChannelNote) || anyNoteHas(notes, "2 "+contentChannelNote) {
		t.Errorf("同一块被重复计数：%v", notes)
	}
}

// Notes() 抽干式：取过一次之后不重复报。
func TestR15ContentChannelNoteDrainedOnce(t *testing.T) {
	d := newStreamDecoder()
	frames := []string{
		`{"type":"response.created","response":{"id":"r1","model":"m"}}`,
		`{"type":"response.reasoning_text.delta","output_index":0,"delta":"INNER"}`,
		`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed"}}`,
	}
	for _, f := range frames {
		if _, err := d.Feed("", f); err != nil {
			t.Fatalf("Feed(%s): %v", f, err)
		}
	}
	if first := d.Notes(); !anyNoteHas(first, "1 "+contentChannelNote) {
		t.Fatalf("首取没报出 content 通道塌缩：%v", first)
	}
	if second := d.Notes(); anyNoteHas(second, contentChannelNote) {
		t.Errorf("同一批被重复报出：%v", second)
	}
}

// 措辞必须落在「流式事件模型没有通道槽位」上，并点名非流式靠 ContentChannel
// 保全——口径与真实处置一致，不许断言「正文丢了」（正文其实保留了）。
func TestR15ContentChannelNoteWording(t *testing.T) {
	_, notes := feedEvents(t,
		`{"type":"response.created","response":{"id":"r1","model":"m"}}`,
		`{"type":"response.reasoning_text.delta","output_index":0,"delta":"INNER"}`,
		`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed"}}`,
	)
	note := ""
	for _, n := range notes {
		if strings.Contains(n, contentChannelNote) {
			note = n
		}
	}
	if note == "" {
		t.Fatalf("没拿到 content 通道注记：%v", notes)
	}
	if !strings.Contains(note, "text is preserved") {
		t.Errorf("注记没说清正文保留：%s", note)
	}
	if !strings.Contains(note, "ContentChannel") {
		t.Errorf("注记没点名非流式的 ContentChannel 槽位：%s", note)
	}
}
