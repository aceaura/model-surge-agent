package responses

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// done 事件携带的是完整终态值而不是新一份内容。只发终态不发增量的上游
// （done-only 网关）整段正文只在终态帧里出现，此前整个事件被丢掉，
// 客户端一个字都收不到。判据与 R 系列工具参数回补一致：只补尚未发出的后缀。
// 引用（annotations 快照）的回补随 Citation 支持一起移植，不在本文件范围。

// feedRaw 喂帧并返回全部 IR 事件（含 Finish），供事件序与计数断言。
func feedRaw(t *testing.T, raw ...string) []ir.Event {
	t.Helper()
	d := newStreamDecoder()
	var out []ir.Event
	for _, r := range raw {
		evs, err := d.Feed("", r)
		if err != nil {
			t.Fatalf("Feed(%s): %v", r, err)
		}
		out = append(out, evs...)
	}
	return append(out, d.Finish()...)
}

func countType(evs []ir.Event, typ ir.EventType) int {
	n := 0
	for _, ev := range evs {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

// textOf 聚合响应里全部文本块的正文。
func textOf(resp *ir.Response) string {
	var b strings.Builder
	for _, blk := range resp.Content {
		if blk.Type == ir.BlockText {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// thinkingOf 聚合响应里全部思考块的正文。
func thinkingOf(resp *ir.Response) string {
	var b strings.Builder
	for _, blk := range resp.Content {
		if blk.Type == ir.BlockThinking && blk.Thinking != nil {
			b.WriteString(blk.Thinking.Text)
		}
	}
	return b.String()
}

// encodeAll 把 IR 事件编给 responses 客户端，返回拼好的帧串。
func encodeAll(t *testing.T, evs ...ir.Event) string {
	t.Helper()
	enc := inboundCodec{}.NewStreamEncoder(nil)
	var out [][]byte
	for _, ev := range evs {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("Encode(%s): %v", ev.Type, err)
		}
		out = append(out, frames...)
	}
	return string(joinFrames(append(out, enc.Finish()...)))
}

const dbMessageAdded = `{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"m1","role":"assistant"}}`

const dbCompleted = `{"type":"response.completed","response":{"id":"r1","status":"completed"}}`

// done-only：三处终态帧都带全文，正文必须落地且只落地一次。
func TestStreamDecodeDoneOnlyTextBackfillsOnce(t *testing.T) {
	evs := feedRaw(t,
		dbMessageAdded,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"整段正文"}`,
		`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"output_text","text":"整段正文"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","content":[{"type":"output_text","text":"整段正文"}]}}`,
		dbCompleted,
	)
	if n := countType(evs, ir.EvBlockStart); n != 1 {
		t.Fatalf("开块数 = %d，want 1：终态帧各自开了一块", n)
	}
	if n := countType(evs, ir.EvTextDelta); n != 1 {
		t.Fatalf("文本增量事件数 = %d，want 1（done 被当成了新一份内容）", n)
	}
	var agg ir.Aggregator
	for _, ev := range evs {
		agg.Add(ev)
	}
	if got := textOf(agg.Response()); got != "整段正文" {
		t.Fatalf("正文 = %q, want %q", got, "整段正文")
	}
}

// 连 content_part.added 都没有、只有 output_item.done 带 content：仍要出正文。
func TestStreamDecodeItemDoneOnlyBackfillsFromContent(t *testing.T) {
	resp, _ := feedEvents(t,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","content":[{"type":"output_text","text":"只有终态"}]}}`,
		dbCompleted,
	)
	if got := textOf(resp); got != "只有终态" {
		t.Fatalf("正文 = %q, want %q", got, "只有终态")
	}
}

// 增量只来了一半：done 只补缺失后缀，不得把已有前缀再发一遍。
func TestStreamDecodePartialDeltaThenDoneAppendsOnlySuffix(t *testing.T) {
	resp, _ := feedEvents(t,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"前半"}`,
		`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"前半后半"}`,
		`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"output_text","text":"前半后半"}}`,
	)
	if got := textOf(resp); got != "前半后半" {
		t.Fatalf("正文 = %q, want %q（重复前缀或丢后缀都算失败）", got, "前半后半")
	}
}

// 增量已给全：done 不得再产出任何增量。
func TestStreamDecodeFullDeltaThenDoneDoesNotDuplicate(t *testing.T) {
	evs := feedRaw(t,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"完整"}`,
		`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"完整"}`,
		`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"output_text","text":"完整"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","content":[{"type":"output_text","text":"完整"}]}}`,
	)
	if n := countType(evs, ir.EvTextDelta); n != 1 {
		t.Fatalf("文本增量事件数 = %d, want 1（done 被当成了新一份内容）", n)
	}
	var agg ir.Aggregator
	for _, ev := range evs {
		agg.Add(ev)
	}
	if got := textOf(agg.Response()); got != "完整" {
		t.Fatalf("正文 = %q, want %q", got, "完整")
	}
}

// 终态与增量分叉（上游自己不一致）：保留已下发的增量，不追加成畸形正文。
func TestStreamDecodeDivergingDoneIsIgnored(t *testing.T) {
	resp, _ := feedEvents(t,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"ABC"}`,
		`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"XYZ"}`,
	)
	if got := textOf(resp); got != "ABC" {
		t.Fatalf("正文 = %q, want 保留已下发的 %q", got, "ABC")
	}
}

// 拒绝正文的终态在 refusal.done / content_part.done / output_item.done 里各出现
// 一次。本协议实现里拒答 part 与正文同键同块，寻址一致才不下发两遍。
func TestStreamDecodeRefusalDoneBackfillsOnce(t *testing.T) {
	evs := feedRaw(t,
		dbMessageAdded,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":""}}`,
		`{"type":"response.refusal.done","output_index":0,"content_index":0,"refusal":"我不能这么做"}`,
		`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":"我不能这么做"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","content":[{"type":"refusal","refusal":"我不能这么做"}]}}`,
		dbCompleted,
	)
	if n := countType(evs, ir.EvBlockStart); n != 1 {
		t.Fatalf("开块数 = %d, want 1", n)
	}
	if n := countType(evs, ir.EvTextDelta); n != 1 {
		t.Fatalf("拒绝增量事件数 = %d, want 1", n)
	}
	var agg ir.Aggregator
	for _, ev := range evs {
		agg.Add(ev)
	}
	resp := agg.Response()
	if got := textOf(resp); got != "我不能这么做" {
		t.Fatalf("拒绝正文 = %q, want %q", got, "我不能这么做")
	}
	if resp.StopReason != ir.StopContentFilter {
		t.Fatalf("终止原因 = %q, want %q（拒答 part 没被认出）", resp.StopReason, ir.StopContentFilter)
	}
}

// 思考正文的终态在 reasoning_summary_text.done，签名在 output_item.done。
// 正文增量必须排在 signature_delta 之前，且 item.done 的 summary 快照不得再补一遍。
func TestStreamDecodeReasoningDoneBackfillsBeforeSignature(t *testing.T) {
	evs := feedRaw(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs1","summary":[]}}`,
		`{"type":"response.reasoning_summary_text.done","output_index":0,"summary_index":0,"text":"思考全文"}`,
		`{"type":"response.reasoning_summary_part.done","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":"思考全文"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs1","summary":[{"type":"summary_text","text":"思考全文"}],"encrypted_content":"SIG"}}`,
	)
	think, sig := -1, -1
	for n, ev := range evs {
		switch ev.Type {
		case ir.EvThinkingDelta:
			if think >= 0 {
				t.Fatalf("思考增量被下发了两次：%+v", evs)
			}
			think = n
			if ev.Text != "思考全文" {
				t.Fatalf("思考正文 = %q, want %q", ev.Text, "思考全文")
			}
		case ir.EvSigDelta:
			sig = n
		}
	}
	if think < 0 || sig < 0 {
		t.Fatalf("事件序列缺项 think=%d sig=%d：%+v", think, sig, evs)
	}
	if think > sig {
		t.Fatalf("思考正文落在了 signature_delta 之后：%+v", evs)
	}
}

// reasoning_summary_part.done 也要能单独把思考正文补齐（有的网关只发这一帧）。
func TestStreamDecodeReasoningSummaryPartDoneBackfills(t *testing.T) {
	resp, _ := feedEvents(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs1","summary":[]}}`,
		`{"type":"response.reasoning_summary_part.done","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":"只有part终态"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs1","summary":[]}}`,
	)
	if got := thinkingOf(resp); got != "只有part终态" {
		t.Fatalf("思考正文 = %q, want %q", got, "只有part终态")
	}
}

// 多 part 的 done-only 流：每个 part 的终态正文各归各块，content_index 就是
// item.content 里的位置。
func TestStreamDecodeMultiPartDoneOnlyBackfillsPerPart(t *testing.T) {
	resp, _ := feedEvents(t,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","content":[`+
			`{"type":"output_text","text":"第一段"},{"type":"output_text","text":"第二段"}]}}`,
		dbCompleted,
	)
	var texts []string
	for _, blk := range resp.Content {
		if blk.Type == ir.BlockText {
			texts = append(texts, blk.Text)
		}
	}
	if len(texts) != 2 {
		t.Fatalf("文本块数 = %d, want 2（多 part 被并成一块）：%+v", len(texts), resp.Content)
	}
	if texts[0] != "第一段" || texts[1] != "第二段" {
		t.Fatalf("正文 = %q/%q, want 第一段/第二段", texts[0], texts[1])
	}
}

// 块已关就不再回补：迟到的终态比已下发内容更长时，追加会落到已定稿的
// item 上（对齐 new-api mergeFinalValue 的 block.Stopped 判据）。
func TestStreamDecodeBackfillAfterCloseIsIgnored(t *testing.T) {
	evs := feedRaw(t,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"A"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","content":[{"type":"output_text","text":"AB"}]}}`,
		// 迟到的终态帧：块已随 item.done 闭合，更长的终态不得再回补。
		`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"ABC"}`,
		dbCompleted,
	)
	if n := countType(evs, ir.EvBlockStart); n != 1 {
		t.Fatalf("开块数 = %d, want 1（关块后又被补开）", n)
	}
	var agg ir.Aggregator
	for _, ev := range evs {
		agg.Add(ev)
	}
	if got := textOf(agg.Response()); got != "AB" {
		t.Fatalf("正文 = %q, want %q（关块后又被追加）", got, "AB")
	}
}

// ---- 单一来源隔离 ----
// 下面每个用例只让一处终态帧携带内容，其余终态帧缺席或为空。
// 少了这组隔离，任一回补点被摘掉都能由别的点兜住，变异检不出来。

func TestStreamDecodeOutputTextDoneIsSoleTextSource(t *testing.T) {
	resp, _ := feedEvents(t,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"甲"}`,
		dbCompleted,
	)
	if got := textOf(resp); got != "甲" {
		t.Fatalf("正文 = %q, want %q", got, "甲")
	}
}

func TestStreamDecodeContentPartDoneIsSoleTextSource(t *testing.T) {
	resp, _ := feedEvents(t,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"output_text","text":"乙"}}`,
		dbCompleted,
	)
	if got := textOf(resp); got != "乙" {
		t.Fatalf("正文 = %q, want %q", got, "乙")
	}
}

func TestStreamDecodeRefusalDoneIsSoleTextSource(t *testing.T) {
	resp, _ := feedEvents(t,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":""}}`,
		`{"type":"response.refusal.done","output_index":0,"content_index":0,"refusal":"丙"}`,
		dbCompleted,
	)
	if got := textOf(resp); got != "丙" {
		t.Fatalf("拒绝正文 = %q, want %q", got, "丙")
	}
}

func TestStreamDecodeReasoningSummaryTextDoneIsSoleSource(t *testing.T) {
	resp, _ := feedEvents(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs1","summary":[]}}`,
		`{"type":"response.reasoning_summary_text.done","output_index":0,"summary_index":0,"text":"丁"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs1","summary":[]}}`,
		dbCompleted,
	)
	if got := thinkingOf(resp); got != "丁" {
		t.Fatalf("思考正文 = %q, want %q", got, "丁")
	}
}

// reasoning_text.done 是原始推理文本的终态，与 summary 走同一条回补路径。
func TestStreamDecodeReasoningTextDoneIsSoleSource(t *testing.T) {
	resp, _ := feedEvents(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs1","summary":[]}}`,
		`{"type":"response.reasoning_text.done","output_index":0,"text":"原始推理"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs1","summary":[]}}`,
		dbCompleted,
	)
	if got := thinkingOf(resp); got != "原始推理" {
		t.Fatalf("思考正文 = %q, want %q", got, "原始推理")
	}
}

// summary 快照只在 output_item.done 里（有的网关不发 reasoning_* 终止帧），
// 且签名必须照常下发。
func TestStreamDecodeItemDoneReasoningSummaryIsSoleSource(t *testing.T) {
	resp, _ := feedEvents(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs1","summary":[]}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs1","summary":[{"type":"summary_text","text":"戊"}],"encrypted_content":"SIG"}}`,
		dbCompleted,
	)
	if got := thinkingOf(resp); got != "戊" {
		t.Fatalf("思考正文 = %q, want %q", got, "戊")
	}
	th := thinkingBlock(t, resp)
	if th.Signature != "SIG" {
		t.Fatalf("签名 = %q, want %q（回补挤掉了签名）", th.Signature, "SIG")
	}
}

// 拒绝与思考的增量也要记账，否则终态回补会把已有前缀再发一遍。
func TestStreamDecodeRefusalPartialDeltaThenDoneAppendsOnlySuffix(t *testing.T) {
	resp, _ := feedEvents(t,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":""}}`,
		`{"type":"response.refusal.delta","output_index":0,"content_index":0,"delta":"我不能"}`,
		`{"type":"response.refusal.done","output_index":0,"content_index":0,"refusal":"我不能这么做"}`,
		dbCompleted,
	)
	if got := textOf(resp); got != "我不能这么做" {
		t.Fatalf("拒绝正文 = %q, want %q", got, "我不能这么做")
	}
}

func TestStreamDecodeReasoningPartialDeltaThenDoneAppendsOnlySuffix(t *testing.T) {
	resp, _ := feedEvents(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs1","summary":[]}}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"先想"}`,
		`{"type":"response.reasoning_summary_text.done","output_index":0,"summary_index":0,"text":"先想后说"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs1","summary":[{"type":"summary_text","text":"先想后说"}]}}`,
		dbCompleted,
	)
	if got := thinkingOf(resp); got != "先想后说" {
		t.Fatalf("思考正文 = %q, want %q", got, "先想后说")
	}
}

// 编码侧：part 级终止帧必须齐全且顺序为 *.done -> content_part.done ->
// output_item.done。只发 output_item.done 的话，按 part 事件关块的下游
// （cc-switch 把 output_text.done 映射成 content_block_stop）永远等不到块结束。
func TestStreamEncodeEmitsPartDoneFramesInOfficialOrder(t *testing.T) {
	s := encodeAll(t,
		ir.Event{Type: ir.EvMessageStart, MessageID: "r1", Model: "gpt"},
		ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "正文"},
		ir.Event{Type: ir.EvBlockStop, Index: 0},
		ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn},
	)
	textDone := strings.Index(s, `"response.output_text.done"`)
	partDone := strings.Index(s, `"response.content_part.done"`)
	itemDone := strings.Index(s, `"response.output_item.done"`)
	if textDone < 0 || partDone < 0 || itemDone < 0 {
		t.Fatalf("缺 part 级终止帧：text.done=%d part.done=%d item.done=%d\n%s", textDone, partDone, itemDone, s)
	}
	if !(textDone < partDone && partDone < itemDone) {
		t.Fatalf("终止帧顺序不对：%d/%d/%d\n%s", textDone, partDone, itemDone, s)
	}
	// 完整正文要在 done 帧里，否则只读终态的下游拿不到内容。
	// 帧体经序号补齐后按字典序序列化，载荷字段排在 type 之前，所以按
	// 「载荷紧邻类型名」断言，而不是从类型名的位置往前切。
	if !strings.Contains(s, `"text":"正文","type":"response.output_text.done"`) {
		t.Fatalf("output_text.done 没带完整终态：\n%s", s)
	}
	if !strings.Contains(s, `"part":{"type":"output_text","text":"正文"},"type":"response.content_part.done"`) {
		t.Fatalf("content_part.done 没带完整终态：\n%s", s)
	}
}

// 思考块的终态是 reasoning_summary_text.done + reasoning_summary_part.done。
func TestStreamEncodeReasoningDoneFrames(t *testing.T) {
	s := encodeAll(t,
		ir.Event{Type: ir.EvMessageStart, MessageID: "r1", Model: "gpt"},
		ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{}}},
		ir.Event{Type: ir.EvThinkingDelta, Index: 0, Text: "想"},
		ir.Event{Type: ir.EvBlockStop, Index: 0},
		ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn},
	)
	sumDone := strings.Index(s, `"response.reasoning_summary_text.done"`)
	partDone := strings.Index(s, `"response.reasoning_summary_part.done"`)
	itemDone := strings.Index(s, `"response.output_item.done"`)
	if sumDone < 0 || partDone < 0 || !(sumDone < partDone && partDone < itemDone) {
		t.Fatalf("思考终止帧缺失或顺序不对：%d/%d/%d\n%s", sumDone, partDone, itemDone, s)
	}
	if !strings.Contains(s, `"text":"想","type":"response.reasoning_summary_text.done"`) {
		t.Fatalf("reasoning_summary_text.done 没带完整思考：\n%s", s)
	}
}

// 端到端：done-only 上游 -> IR -> responses 客户端，正文只以一次增量下发，
// 其余出现都是各帧的完整终态。
func TestResponsesRoundTripDoneOnlyKeepsTextOnce(t *testing.T) {
	evs := feedRaw(t,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"整段正文"}`,
		`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"output_text","text":"整段正文"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","content":[{"type":"output_text","text":"整段正文"}]}}`,
		dbCompleted,
	)
	s := encodeAll(t, evs...)
	// 一份增量 + output_text.done + content_part.done + output_item.done + completed.output
	if got := strings.Count(s, "整段正文"); got != 5 {
		t.Fatalf("正文出现 %d 次, want 5（一份增量四份终态）：\n%s", got, s)
	}
	if strings.Count(s, `"response.output_text.delta"`) != 1 {
		t.Fatalf("回补的正文没有以增量形式下发给客户端：\n%s", s)
	}
}

// 端到端：正常全增量流往返后正文不得翻倍。
func TestResponsesRoundTripNormalStreamNoDuplication(t *testing.T) {
	evs := feedRaw(t,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"完整"}`,
		`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"完整"}`,
		`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"output_text","text":"完整"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","content":[{"type":"output_text","text":"完整"}]}}`,
		dbCompleted,
	)
	s := encodeAll(t, evs...)
	if strings.Contains(s, "完整完整") {
		t.Fatalf("正文被翻倍：\n%s", s)
	}
	if strings.Count(s, `"response.output_text.delta"`) != 1 {
		t.Fatalf("增量帧数不对：\n%s", s)
	}
}
