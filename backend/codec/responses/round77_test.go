package responses

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
)

// 轮次77：responses 非流式整份响应解码（DecodeResponseLossy）在 itemReasoning
// 分支用 joinSummary 把官方 summary 数组的多段文本首尾相接成一条 IR 思考块。文本
// 保全了，但「几段、各段边界、summary_index 寻址」丢失——这与流式对 summary_index>0
// 的帧计数（d.mergedSummary → Notes()）是**同一次折叠**。此前只有流式出注记，非流式
// 的注记集（hosted/logprobs/nonURLCites/phases/itemStatuses）没有这一条，而 codec.go
// 里 DecodeResponseLossy 的注释明写「与流式的 Notes() 对称」：这是一处规则 b
// （流式/非流式同损同措辞）违背。
//
// 修复：把流式内联的 Sprintf 提为共享 codec.ResponseMergedSummaryNote，两条路径共用
// 以逐字节对齐措辞；非流式在 itemReasoning 分支按「额外段数 len(summary)-1」计数
// （聚合响应的 wireSummary 不带 index，与流式「index>0 的帧数」等价），>0 时出注记。
// 单段（len<=1）无折叠、不计数（规则 a：无真实丢弃不得误报）。
//
// 指纹子串 "reasoning summary frame(s)" 为 ResponseMergedSummaryNote 独有。

const r77fingerprint = "reasoning summary frame(s)"

// 非流式：单条 reasoning 带两段 summary → 折叠成一条，注记报出且计数为额外段数 1。
func TestNonStreamMergedSummaryNoted(t *testing.T) {
	body := `{"id":"r1","object":"response","status":"completed","output":[
      {"type":"reasoning","id":"rs_1","summary":[
        {"type":"summary_text","text":"第一段"},
        {"type":"summary_text","text":"第二段"}]}]}`
	resp, notes, err := DecodeResponseLossy([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	// 文本仍保全（两段相接），只是丢了边界——注记描述的是边界丢失，不是文本丢失。
	if resp == nil || len(resp.Content) != 1 {
		t.Fatalf("应解出一个思考块：%+v", resp)
	}
	if th := resp.Content[0].Thinking; th == nil || th.Text != "第一段第二段" {
		t.Fatalf("思考正文应保全为相接文本：%+v", th)
	}
	want := codec.ResponseMergedSummaryNote(1)
	if findNote(notes, r77fingerprint) != want {
		t.Errorf("合并注记缺失或计数错误：\n got=%v\n want 含 %q", notes, want)
	}
}

// 非流式：三段 summary → 计数为额外段数 2。
func TestNonStreamMergedSummaryCountsExtraParts(t *testing.T) {
	body := `{"id":"r1","object":"response","status":"completed","output":[
      {"type":"reasoning","id":"rs_1","summary":[
        {"type":"summary_text","text":"a"},
        {"type":"summary_text","text":"b"},
        {"type":"summary_text","text":"c"}]}]}`
	_, notes, err := DecodeResponseLossy([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if findNote(notes, r77fingerprint) != codec.ResponseMergedSummaryNote(2) {
		t.Errorf("三段应计额外 2 段：%v", notes)
	}
}

// 非流式：单段 summary → 无折叠、无丢失，不得误报（规则 a）。
func TestNonStreamSingleSummaryQuiet(t *testing.T) {
	body := `{"id":"r1","object":"response","status":"completed","output":[
      {"type":"reasoning","id":"rs_1","summary":[
        {"type":"summary_text","text":"只有一段"}]}]}`
	_, notes, err := DecodeResponseLossy([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if findNote(notes, r77fingerprint) != "" {
		t.Errorf("单段 summary 误报折叠注记：%v", notes)
	}
}

// 非流式：summary 为空、正文走 content（reasoning_text）通道 → 无摘要折叠，不误报。
func TestNonStreamContentChannelSummaryQuiet(t *testing.T) {
	body := `{"id":"r1","object":"response","status":"completed","output":[
      {"type":"reasoning","id":"rs_1","summary":[],
       "content":[{"type":"reasoning_text","text":"思考正文"}]}]}`
	_, notes, err := DecodeResponseLossy([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if findNote(notes, r77fingerprint) != "" {
		t.Errorf("空摘要（content 通道）误报折叠注记：%v", notes)
	}
}

// 规则 b：同一损类流式与非流式措辞逐字一致——两条路径各解一份等价的两段 reasoning，
// 比对注记文本相同（共享 codec.ResponseMergedSummaryNote 保证）。
func TestMergedSummaryStreamingMatchesNonStreamingWording(t *testing.T) {
	_, streamNotes := feedRawNotes(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`,
		`{"type":"response.reasoning_summary_part.added","output_index":0,"item_id":"rs_1","summary_index":0,"part":{"type":"summary_text","text":""}}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"item_id":"rs_1","summary_index":0,"delta":"第一段"}`,
		// 一段额外（index=1）：delta/text.done/part.done 各计一次，流式合计 3。
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"item_id":"rs_1","summary_index":1,"delta":"第二段"}`,
		`{"type":"response.reasoning_summary_text.done","output_index":0,"item_id":"rs_1","summary_index":1,"text":"第二段"}`,
		`{"type":"response.reasoning_summary_part.done","output_index":0,"item_id":"rs_1","summary_index":1,"part":{"type":"summary_text","text":"第二段"}}`)
	body := `{"id":"r1","object":"response","status":"completed","output":[
      {"type":"reasoning","id":"rs_1","summary":[
        {"type":"summary_text","text":"第一段"},
        {"type":"summary_text","text":"第二段"}]}]}`
	_, nonStreamNotes, err := DecodeResponseLossy([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	s := findNote(streamNotes, r77fingerprint)
	n := findNote(nonStreamNotes, r77fingerprint)
	if s == "" || n == "" {
		t.Fatalf("两条路径都应报出合并注记：stream=%v nonstream=%v", streamNotes, nonStreamNotes)
	}
	// 计数量纲不同（流式数 index>0 的帧、非流式数额外段数），但措辞模板必须同源：
	// 去掉数字后前缀一致，且都以共享函数的固定尾串收束。
	if !strings.HasSuffix(s, "of a multi-part reasoning item are not") ||
		!strings.HasSuffix(n, "of a multi-part reasoning item are not") {
		t.Errorf("措辞尾串不一致：\n stream=%q\n nonstream=%q", s, n)
	}
	if !strings.HasPrefix(s, "merged ") || !strings.HasPrefix(n, "merged ") {
		t.Errorf("措辞前缀不一致：\n stream=%q\n nonstream=%q", s, n)
	}
}
