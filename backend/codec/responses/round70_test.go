package responses

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次70（responses 客户端侧）：文档块的两项配置（context 用途旁注 /
// citations.enabled 引用开关）是独立于附件本体的一维。非流式 EncodeResponseLossy
// 早已同时报出本体（MediaOutputDropNote）与配置（DocumentConfigDropNote）两条注记，
// 但流式编码器只把文档块折进 droppedFiles 报本体、从不检查 DocConfigOf——同一损类
// 在流式路径少报一维，违反规则 b（流式/非流式同损同报）。本轮在 EvBlockStart 丢弃
// 媒体块时经 countDocConfig 累加配置维度，Notes() 收尾补出 DocumentConfigDropNote。
//
// 可达性同 chat：anthropic 上游流式 document 块经共用块解码器把配置落进 ir.Media，
// anthropic 是承载族（不报），只有 chat / responses 两个外族入站编码器需要补报。

const r70marker = "per-document configuration"

func r70bool(b bool) *bool { return &b }

func r70doc(ctx string, cites *bool) ir.Block {
	return ir.Block{Type: ir.BlockDocument, Media: &ir.Media{
		MediaType: "application/pdf", Data: "JVBERi0", Context: ctx, CitationsEnabled: cites,
	}}
}

func r70docPtr(ctx string, cites *bool) *ir.Block {
	b := r70doc(ctx, cites)
	return &b
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
func TestR70RespReplayDocConfigNoted(t *testing.T) {
	notes := r70replayNotes(t, r70resp(r70doc("summary", r70bool(true))))
	if r70findNote(notes, r70marker) == "" {
		t.Errorf("投影路径没报文档配置丢弃：%v", notes)
	}
}

// 真流式：直接喂 EvBlockStart（骨架 Media 带配置），报出。
func TestR70RespTrueStreamDocConfigNoted(t *testing.T) {
	enc := newStreamEncoder()
	ev := ir.Event{Type: ir.EvBlockStart, Index: 0, Block: r70docPtr("ctx", r70bool(false))}
	if _, err := enc.Encode(ev); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if r70findNote(enc.Notes(), r70marker) == "" {
		t.Errorf("真流式没报文档配置丢弃")
	}
}

// 规则 b：非流式与投影流式两条路径的文档配置注记措辞逐字一致。
func TestR70RespStreamNonStreamWordingIdentical(t *testing.T) {
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

// 假阳性防线：文档块本体照旧丢弃并报媒体注记，但没带任何配置时绝不额外报
// DocumentConfigDropNote（规则 a：注记当且仅当真实丢弃）。
func TestR70RespReplayDocWithoutConfigSilent(t *testing.T) {
	notes := r70replayNotes(t, r70resp(r70doc("", nil)))
	if r70findNote(notes, r70marker) != "" {
		t.Errorf("无配置的文档块不该报配置丢弃：%v", notes)
	}
	if len(notes) == 0 {
		t.Errorf("文档块本体仍应报媒体丢弃，注记不该全空")
	}
}

// 只带 context：渲染「usage context」，不渲染「citation switch」。
func TestR70RespReplayCtxOnly(t *testing.T) {
	s := r70findNote(r70replayNotes(t, r70resp(r70doc("guidance", nil))), r70marker)
	if !strings.Contains(s, "usage context") || strings.Contains(s, "citation switch") {
		t.Errorf("只带 context 的措辞不对：%q", s)
	}
}

// 只带 citations.enabled（显式 false 也算携带开关）：渲染「citation switch」，
// 不渲染「usage context」。
func TestR70RespReplayCitesOnly(t *testing.T) {
	s := r70findNote(r70replayNotes(t, r70resp(r70doc("", r70bool(false)))), r70marker)
	if !strings.Contains(s, "citation switch") || strings.Contains(s, "usage context") {
		t.Errorf("只带引用的措辞不对：%q", s)
	}
}

// 多块累计：两个带配置的文档块汇成一条，计数为 2。
func TestR70RespReplayMultipleCounted(t *testing.T) {
	notes := r70replayNotes(t, r70resp(
		r70doc("a", r70bool(true)), r70doc("b", r70bool(true))))
	s := r70findNote(notes, r70marker)
	if !strings.Contains(s, "2 document(s)") {
		t.Errorf("两块带配置文档应汇成一条报 2：%v", notes)
	}
}
