package responses

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次59 G1：整份响应投影（ir.ResponseEvents）→ responses 流式编码器这条路径上，
// content 通道推理块（Thinking.ContentChannel=true）被编码器一律渲染成 summary
// 通道（reasoning_summary_text.delta），此前静默无注记。
//
// 三条路径的处置必须对齐：
//   - 真流式：解码器侧按 reasoning_text.delta 帧计数报出（round15_test）；
//   - 非流式：编码器原样保全 content 通道，无损不报（r110_test）；
//   - 投影（本轮）：解码走非流式 DecodeResponseLossy（保全 ContentChannel 且不报），
//     重放骨架带着 ContentChannel 进流式编码器，改标只可能在这里被发现——
//     不报就与真流式同损不同报，违反规则 b。
//
// 措辞共用 codec.ContentChannelStreamNote，与解码器侧逐字一致。

// encodeReplay 把一份完整响应投影成事件、喂进流式编码器，收回 Notes()。
// 这正是 pipeline/upstream.go 在「上游忽略 stream:true 回整份 JSON」时走的路径。
func encodeReplay(t *testing.T, resp *ir.Response) []string {
	t.Helper()
	enc := newStreamEncoder()
	for _, ev := range ir.ResponseEvents(resp) {
		if _, err := enc.Encode(ev); err != nil {
			t.Fatalf("Encode(%v): %v", ev.Type, err)
		}
	}
	return enc.Notes()
}

// findNote 返回 notes 里第一条含 sub 的注记，找不到返回空串。
func findNote(notes []string, sub string) string {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return n
		}
	}
	return ""
}

// 投影路径：content 通道推理块 → 编码器侧报出，计数 1。
func TestR59ReplayContentChannelNoted(t *testing.T) {
	resp := &ir.Response{ID: "r1", Model: "m", Content: []ir.Block{
		{Type: ir.BlockThinking, Thinking: &ir.Thinking{
			Text: "INNER", ContentChannel: true, ItemID: "rs_1"}},
	}}
	notes := encodeReplay(t, resp)
	if !anyNoteHas(notes, "1 "+contentChannelNote) {
		t.Errorf("投影路径 content 通道推理没被编码器报出：%v", notes)
	}
}

// 投影路径：summary 通道（ContentChannel=false，历史默认）不许误报。
func TestR59ReplaySummaryChannelNoNote(t *testing.T) {
	resp := &ir.Response{ID: "r1", Model: "m", Content: []ir.Block{
		{Type: ir.BlockThinking, Thinking: &ir.Thinking{
			Text: "SUM", ContentChannel: false, ItemID: "rs_1"}},
	}}
	if notes := encodeReplay(t, resp); anyNoteHas(notes, contentChannelNote) {
		t.Errorf("summary 通道被误报成 content 通道塌缩：%v", notes)
	}
}

// 投影路径：多个 content 通道块按数累计。
func TestR59ReplayMultipleContentChannelCounted(t *testing.T) {
	resp := &ir.Response{ID: "r1", Model: "m", Content: []ir.Block{
		{Type: ir.BlockThinking, Thinking: &ir.Thinking{
			Text: "A", ContentChannel: true, ItemID: "rs_1"}},
		{Type: ir.BlockText, Text: "中间"},
		{Type: ir.BlockThinking, Thinking: &ir.Thinking{
			Text: "B", ContentChannel: true, ItemID: "rs_2"}},
	}}
	if notes := encodeReplay(t, resp); !anyNoteHas(notes, "2 "+contentChannelNote) {
		t.Errorf("两个 content 通道块没按数报出：%v", notes)
	}
}

// 规则 b 硬钉：同一份 content 通道推理，真流式（解码器侧）与投影（编码器侧）
// 两条路径报出的注记必须逐字一致——共用 codec.ContentChannelStreamNote。
func TestR59StreamAndReplayWordingIdentical(t *testing.T) {
	// 真流式：reasoning_text.delta 帧 → 解码器侧注记。
	_, decNotes := feedEvents(t,
		`{"type":"response.created","response":{"id":"r1","model":"m"}}`,
		`{"type":"response.reasoning_text.delta","output_index":0,"delta":"INNER"}`,
		`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed"}}`,
	)
	decNote := findNote(decNotes, contentChannelNote)
	if decNote == "" {
		t.Fatalf("真流式解码器没报出 content 通道注记：%v", decNotes)
	}

	// 投影：整份响应 → 编码器侧注记。
	encNote := findNote(encodeReplay(t, &ir.Response{ID: "r1", Model: "m",
		Content: []ir.Block{{Type: ir.BlockThinking, Thinking: &ir.Thinking{
			Text: "INNER", ContentChannel: true, ItemID: "rs_1"}}}}), contentChannelNote)
	if encNote == "" {
		t.Fatalf("投影编码器没报出 content 通道注记")
	}

	if decNote != encNote {
		t.Errorf("两条流式路径措辞不一致（违反规则 b）：\n解码器=%q\n编码器=%q", decNote, encNote)
	}
}

// 去重：同一块索引的 block_start 重复到达只计一次（openBlock 顶部 items[index]
// 已存在即返回）。直接驱动编码器复现投影路径不会自然产生的重复开块。
func TestR59SameIndexBlockStartDeduped(t *testing.T) {
	enc := newStreamEncoder()
	skel := func() ir.Event {
		return ir.Event{Type: ir.EvBlockStart, Index: 0,
			Block: &ir.Block{Type: ir.BlockThinking,
				Thinking: &ir.Thinking{ContentChannel: true, ItemID: "rs_1"}}}
	}
	if _, err := enc.Encode(skel()); err != nil {
		t.Fatalf("Encode#1: %v", err)
	}
	if _, err := enc.Encode(skel()); err != nil {
		t.Fatalf("Encode#2: %v", err)
	}
	notes := enc.Notes()
	if !anyNoteHas(notes, "1 "+contentChannelNote) {
		t.Errorf("重复开块没去重成 1：%v", notes)
	}
	if anyNoteHas(notes, "2 "+contentChannelNote) {
		t.Errorf("重复开块被重复计数：%v", notes)
	}
}

// Notes() 抽干式：编码器侧取过一次之后不重复报（与解码器侧同纪律）。
func TestR59EncoderNoteDrainedOnce(t *testing.T) {
	enc := newStreamEncoder()
	for _, ev := range ir.ResponseEvents(&ir.Response{ID: "r1", Model: "m",
		Content: []ir.Block{{Type: ir.BlockThinking, Thinking: &ir.Thinking{
			Text: "INNER", ContentChannel: true, ItemID: "rs_1"}}}}) {
		if _, err := enc.Encode(ev); err != nil {
			t.Fatalf("Encode: %v", err)
		}
	}
	if first := enc.Notes(); !anyNoteHas(first, "1 "+contentChannelNote) {
		t.Fatalf("首取没报出：%v", first)
	}
	if second := enc.Notes(); anyNoteHas(second, contentChannelNote) {
		t.Errorf("同一批被重复报出：%v", second)
	}
}
