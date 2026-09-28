package chatcompletions

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次70（chat 客户端侧）：文档块的两项配置（context 用途旁注 / citations.enabled
// 引用开关）是独立于附件本体的一维。非流式 EncodeResponseLossy 早已同时报出本体
// （MediaOutputDropNote）与配置（DocumentConfigDropNote）两条注记，但流式编码器
// 只把文档块折进 droppedFiles 报本体、从不检查 DocConfigOf——同一损类在流式路径
// 少报一维，违反规则 b（流式/非流式同损同报）。本轮在 EvBlockStart 丢弃媒体块时
// 经 countDocConfig 累加配置维度，Notes() 收尾补出 DocumentConfigDropNote。
//
// 可达性：anthropic 上游流式 content_block_start 带 document 块时，
// decode_stream.go 经 decodeRawBlock→decodeBlock 把 context/citations 落进
// ir.Media（decode_request.go:365-366，请求/响应共用同一块解码器），故响应侧
// 文档块确可携带配置；anthropic 是承载族（自家编码器保全、不报），只有 chat /
// responses 两个外族入站编码器需要补报。

const r70marker = "per-document configuration"

func r70bool(b bool) *bool { return &b }

// r70doc 造一个模型产出的文档块，携带指定的两项配置。
func r70doc(ctx string, cites *bool) ir.Block {
	return ir.Block{Type: ir.BlockDocument, Media: &ir.Media{
		MediaType: "application/pdf", Data: "JVBERi0", Context: ctx, CitationsEnabled: cites,
	}}
}

func r70resp(blocks ...ir.Block) *ir.Response {
	return &ir.Response{ID: "r1", Model: "m", Content: blocks}
}

func r70findNote(notes []string, marker string) string {
	for _, s := range notes {
		if strings.Contains(s, marker) {
			return s
		}
	}
	return ""
}

// r70replayNotes 驱动整份响应投影（ir.ResponseEvents）走流式编码器，收回注记。
func r70replayNotes(t *testing.T, resp *ir.Response) []string {
	t.Helper()
	enc := newStreamEncoder()
	for _, ev := range ir.ResponseEvents(resp) {
		if _, err := enc.Encode(ev); err != nil {
			t.Fatalf("Encode(%v): %v", ev.Type, err)
		}
	}
	return enc.Notes()
}

// 投影路径：整份响应重放，文档块带 context+citations，流式编码器报出配置维度。
func TestR70ChatReplayDocConfigNoted(t *testing.T) {
	notes := r70replayNotes(t, r70resp(r70doc("summary", r70bool(true))))
	if r70findNote(notes, r70marker) == "" {
		t.Errorf("投影路径没报文档配置丢弃：%v", notes)
	}
}

// 真流式：直接喂 EvBlockStart（骨架 Media 带配置），报出。
func TestR70ChatTrueStreamDocConfigNoted(t *testing.T) {
	enc := newStreamEncoder()
	ev := ir.Event{Type: ir.EvBlockStart, Index: 0, Block: r70docPtr("ctx", r70bool(false))}
	if _, err := enc.Encode(ev); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if r70findNote(enc.Notes(), r70marker) == "" {
		t.Errorf("真流式没报文档配置丢弃")
	}
}

func r70docPtr(ctx string, cites *bool) *ir.Block {
	b := r70doc(ctx, cites)
	return &b
}

// 规则 b：非流式与投影流式两条路径的文档配置注记措辞逐字一致
// （共用 codec.DocumentConfigDropNote）。
func TestR70ChatStreamNonStreamWordingIdentical(t *testing.T) {
	_, nsNotes, err := inboundCodec{}.EncodeResponseLossy(r70resp(r70doc("summary", r70bool(true))))
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	ns := r70findNote(nsNotes, r70marker)
	st := r70findNote(r70replayNotes(t, r70resp(r70doc("summary", r70bool(true)))), r70marker)
	if ns == "" || st == "" {
		t.Fatalf("两条路径都应报出：非流式=%q 流式=%q", ns, st)
	}
	if ns != st {
		t.Errorf("非流式与流式措辞不一致（违反规则 b）：\n非流式=%q\n流式=%q", ns, st)
	}
}

// 假阳性防线：文档块本体照旧丢弃并报 MediaOutputDropNote，但没带任何配置时
// 绝不额外报 DocumentConfigDropNote（规则 a：注记当且仅当真实丢弃）。
func TestR70ChatReplayDocWithoutConfigSilent(t *testing.T) {
	notes := r70replayNotes(t, r70resp(r70doc("", nil)))
	if r70findNote(notes, r70marker) != "" {
		t.Errorf("无配置的文档块不该报配置丢弃：%v", notes)
	}
	if len(notes) == 0 {
		t.Errorf("文档块本体仍应报媒体丢弃，注记不该全空")
	}
}

// 只带 context：措辞渲染「usage context」，不渲染「citation switch」。
func TestR70ChatReplayCtxOnly(t *testing.T) {
	s := r70findNote(r70replayNotes(t, r70resp(r70doc("guidance", nil))), r70marker)
	if !strings.Contains(s, "usage context") || strings.Contains(s, "citation switch") {
		t.Errorf("只带 context 的措辞不对：%q", s)
	}
}

// 只带 citations.enabled（显式 false 也算携带开关）：渲染「citation switch」，
// 不渲染「usage context」。
func TestR70ChatReplayCitesOnly(t *testing.T) {
	s := r70findNote(r70replayNotes(t, r70resp(r70doc("", r70bool(false)))), r70marker)
	if !strings.Contains(s, "citation switch") || strings.Contains(s, "usage context") {
		t.Errorf("只带引用的措辞不对：%q", s)
	}
}

// 多块累计：两个带配置的文档块汇成一条，计数为 2。
func TestR70ChatReplayMultipleCounted(t *testing.T) {
	notes := r70replayNotes(t, r70resp(
		r70doc("a", r70bool(true)), r70doc("b", r70bool(true))))
	s := r70findNote(notes, r70marker)
	if !strings.Contains(s, "2 document(s)") {
		t.Errorf("两块带配置文档应汇成一条报 2：%v", notes)
	}
}
