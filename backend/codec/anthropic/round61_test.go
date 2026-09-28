package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次61：responses 上游 content 通道推理（reasoning_text 原文，ir.Thinking.
// ContentChannel=true）跨族投给 anthropic 客户端时，正文写回 thinking 块逐字保留，
// 但本协议思考块无通道维度，「原始推理 vs 用户摘要」provenance 丢失。此前响应侧静默
//（请求侧轮次43 已报、responses 同族轮次15/59 已报），本轮补齐 anthropic 响应侧：
// 非流式（EncodeResponseLossy）与整份响应投影（ir.ResponseEvents → 流式编码器）两条
// 路径同损同措辞（规则 b），且真流式骨架不带 ContentChannel 时不误计（不与解码器双报）。

const r61marker = "content-channel marker"

func r61resp(cc bool, n int) *ir.Response {
	blocks := make([]ir.Block, 0, n)
	for i := 0; i < n; i++ {
		blocks = append(blocks, ir.Block{Type: ir.BlockThinking,
			Thinking: &ir.Thinking{Text: "INNER", ContentChannel: cc}})
	}
	return &ir.Response{ID: "r1", Model: "m", Content: blocks}
}

func r61markerNote(notes []string) string {
	for _, s := range notes {
		if strings.Contains(s, r61marker) {
			return s
		}
	}
	return ""
}

func r61replayNotes(t *testing.T, resp *ir.Response) []string {
	t.Helper()
	enc := newStreamEncoder()
	for _, ev := range ir.ResponseEvents(resp) {
		if _, err := enc.Encode(ev); err != nil {
			t.Fatalf("Encode(%v): %v", ev.Type, err)
		}
	}
	return enc.Notes()
}

// 非流式：content 通道推理投给 anthropic 客户端，报出通道标记丢弃。
func TestR61AnthropicNonStreamContentChannelNoted(t *testing.T) {
	_, notes, err := inboundCodec{}.EncodeResponseLossy(r61resp(true, 1))
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if !anyNoteHas(notes, r61marker) {
		t.Errorf("非流式没报 content 通道丢弃：%v", notes)
	}
}

// 非流式：summary 通道（ContentChannel=false）不误报。
func TestR61AnthropicNonStreamSummaryNotNoted(t *testing.T) {
	_, notes, err := inboundCodec{}.EncodeResponseLossy(r61resp(false, 1))
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if anyNoteHas(notes, r61marker) {
		t.Errorf("summary 通道被误报：%v", notes)
	}
}

// 投影路径：整份响应经 ir.ResponseEvents 重放，骨架带 ContentChannel，流式编码器报出。
func TestR61AnthropicReplayContentChannelNoted(t *testing.T) {
	notes := r61replayNotes(t, r61resp(true, 1))
	if !anyNoteHas(notes, r61marker) {
		t.Errorf("投影路径没报 content 通道丢弃：%v", notes)
	}
}

// 规则 b：非流式与投影流式两条路径措辞逐字一致（共用 ContentChannelCrossFamilyNote）。
func TestR61AnthropicStreamNonStreamWordingIdentical(t *testing.T) {
	_, nsNotes, err := inboundCodec{}.EncodeResponseLossy(r61resp(true, 1))
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	ns := r61markerNote(nsNotes)
	st := r61markerNote(r61replayNotes(t, r61resp(true, 1)))
	if ns == "" || st == "" {
		t.Fatalf("两条路径都应报出：非流式=%q 流式=%q", ns, st)
	}
	if ns != st {
		t.Errorf("非流式与流式措辞不一致（违反规则 b）：\n非流式=%q\n流式=%q", ns, st)
	}
}

// 真流式骨架不带 ContentChannel（responses 解码器已在解码侧计数报出），编码器不得
// 再次计数，否则同一损耗双报。
func TestR61AnthropicTrueStreamSkeletonNoDoubleNote(t *testing.T) {
	enc := newStreamEncoder()
	ev := ir.Event{Type: ir.EvBlockStart, Index: 0,
		Block: &ir.Block{Type: ir.BlockThinking,
			Thinking: &ir.Thinking{SignatureFrom: "responses", ItemID: "rs_1"}}}
	if _, err := enc.Encode(ev); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if anyNoteHas(enc.Notes(), r61marker) {
		t.Errorf("真流式骨架（ContentChannel=false）被误计，会与解码器双报")
	}
}

// Notes() 抽干式：取过一次之后不重复报。
func TestR61AnthropicNoteDrainedOnce(t *testing.T) {
	enc := newStreamEncoder()
	for _, ev := range ir.ResponseEvents(r61resp(true, 1)) {
		if _, err := enc.Encode(ev); err != nil {
			t.Fatalf("Encode: %v", err)
		}
	}
	if first := enc.Notes(); !anyNoteHas(first, r61marker) {
		t.Fatalf("首取没报出：%v", first)
	}
	if second := enc.Notes(); anyNoteHas(second, r61marker) {
		t.Errorf("同一批被重复报出：%v", second)
	}
}

// 多块投影累计计数。
func TestR61AnthropicReplayMultipleCounted(t *testing.T) {
	notes := r61replayNotes(t, r61resp(true, 2))
	if !anyNoteHas(notes, "2 "+r61marker) && !strings.Contains(r61markerNote(notes), "2 reasoning block(s)") {
		t.Errorf("两块 content 通道应汇成一条报 2：%v", notes)
	}
}
