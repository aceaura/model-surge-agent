package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次63：上游响应里的拒绝正文块（chat/responses 解码器产出 ir.BlockRefusal）跨族
// 投给 anthropic 客户端时，本协议没有独立 refusal 槽位（只有 stop_reason=refusal），
// encodeBlock 把正文降级成普通 text 块——正文逐字保留，但「这是模型拒绝」的标记丢失，
// 客户端分不出拒答与正常正文。此前响应侧静默（请求侧 countRequestRefusals 的
// !caps.Refusal 门控已报），本轮补齐 anthropic 响应侧：非流式（EncodeResponseLossy）
// 与流式（真流式 + 整份响应投影）两条路径同损同措辞（规则 b），且普通文本块不误计。

const r63marker = "refusal(s) from the model output into plain text"

func r63resp(n int) *ir.Response {
	blocks := make([]ir.Block, 0, n)
	for i := 0; i < n; i++ {
		blocks = append(blocks, ir.Block{Type: ir.BlockRefusal, Text: "我不能这么做"})
	}
	return &ir.Response{ID: "r1", Model: "m", Content: blocks}
}

func r63markerNote(notes []string) string {
	for _, s := range notes {
		if strings.Contains(s, r63marker) {
			return s
		}
	}
	return ""
}

func r63replayNotes(t *testing.T, resp *ir.Response) []string {
	t.Helper()
	enc := newStreamEncoder()
	for _, ev := range ir.ResponseEvents(resp) {
		if _, err := enc.Encode(ev); err != nil {
			t.Fatalf("Encode(%v): %v", ev.Type, err)
		}
	}
	return enc.Notes()
}

// 非流式：拒绝正文投给 anthropic 客户端，报出并进普通文本。
func TestR63AnthropicNonStreamRefusalNoted(t *testing.T) {
	_, notes, err := inboundCodec{}.EncodeResponseLossy(r63resp(1))
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if !anyNoteHas(notes, r63marker) {
		t.Errorf("非流式没报拒绝并进文本：%v", notes)
	}
}

// 非流式：正文逐字保留（降级成 text 块，不是丢弃）。
func TestR63AnthropicNonStreamRefusalTextPreserved(t *testing.T) {
	out, _, err := inboundCodec{}.EncodeResponseLossy(r63resp(1))
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if !strings.Contains(string(out), "我不能这么做") {
		t.Errorf("拒绝正文没逐字保留：%s", out)
	}
}

// 非流式：纯文本响应不误报。
func TestR63AnthropicNonStreamTextNotNoted(t *testing.T) {
	resp := &ir.Response{ID: "r1", Model: "m",
		Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}
	_, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if anyNoteHas(notes, r63marker) {
		t.Errorf("纯文本被误报拒绝并进：%v", notes)
	}
}

// 投影路径：整份响应经 ir.ResponseEvents 重放，骨架带 BlockRefusal，流式编码器报出。
func TestR63AnthropicReplayRefusalNoted(t *testing.T) {
	notes := r63replayNotes(t, r63resp(1))
	if !anyNoteHas(notes, r63marker) {
		t.Errorf("投影路径没报拒绝并进文本：%v", notes)
	}
}

// 真流式：解码器产出 BlockRefusal 骨架，流式编码器报出。
func TestR63AnthropicTrueStreamRefusalNoted(t *testing.T) {
	enc := newStreamEncoder()
	ev := ir.Event{Type: ir.EvBlockStart, Index: 0,
		Block: &ir.Block{Type: ir.BlockRefusal}}
	if _, err := enc.Encode(ev); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !anyNoteHas(enc.Notes(), r63marker) {
		t.Errorf("真流式没报拒绝并进文本")
	}
}

// 规则 b：非流式与流式两条路径措辞逐字一致（共用 ResponseRefusalMergeNote）。
func TestR63AnthropicStreamNonStreamWordingIdentical(t *testing.T) {
	_, nsNotes, err := inboundCodec{}.EncodeResponseLossy(r63resp(1))
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	ns := r63markerNote(nsNotes)
	st := r63markerNote(r63replayNotes(t, r63resp(1)))
	if ns == "" || st == "" {
		t.Fatalf("两条路径都应报出：非流式=%q 流式=%q", ns, st)
	}
	if ns != st {
		t.Errorf("非流式与流式措辞不一致（违反规则 b）：\n非流式=%q\n流式=%q", ns, st)
	}
}

// Notes() 抽干式：取过一次之后不重复报。
func TestR63AnthropicNoteDrainedOnce(t *testing.T) {
	enc := newStreamEncoder()
	for _, ev := range ir.ResponseEvents(r63resp(1)) {
		if _, err := enc.Encode(ev); err != nil {
			t.Fatalf("Encode: %v", err)
		}
	}
	if first := enc.Notes(); !anyNoteHas(first, r63marker) {
		t.Fatalf("首取没报出：%v", first)
	}
	if second := enc.Notes(); anyNoteHas(second, r63marker) {
		t.Errorf("同一批被重复报出：%v", second)
	}
}

// 多块投影累计计数。
func TestR63AnthropicReplayMultipleCounted(t *testing.T) {
	notes := r63replayNotes(t, r63resp(2))
	if !strings.Contains(r63markerNote(notes), "2 refusal(s)") {
		t.Errorf("两块拒绝应汇成一条报 2：%v", notes)
	}
}
