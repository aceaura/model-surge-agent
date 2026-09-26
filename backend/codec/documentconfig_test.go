package codec_test

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	_ "github.com/aceaura/model-surge-agent/backend/codec/anthropic"
	_ "github.com/aceaura/model-surge-agent/backend/codec/chatcompletions"
	_ "github.com/aceaura/model-surge-agent/backend/codec/gemini"
	_ "github.com/aceaura/model-surge-agent/backend/codec/responses"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// #62：anthropic document 块的两项配置（context 用途旁注与 citations.enabled
// 引用开关）跨族损耗注记。外族附件槽位只装文件本身，两项配置必丢，此前完全
// 静默——客户端开了文档引用却一条都收不到，也看不到任何迹象。请求侧
// DescribeLossy 与响应侧 EncodeResponseLossy 两条通道都要报出。
//
// 对应旧仓 4689a8a。

func docBlock(ctx string, cites *bool) ir.Block {
	return ir.Block{Type: ir.BlockDocument, Media: &ir.Media{
		MediaType: "application/pdf", Data: "JVBERi0", Context: ctx, CitationsEnabled: cites,
	}}
}

// 请求侧诊断：历史里带文档配置时外族出站报丢失，anthropic 静默，缺席静默。
// 配置内容属客户端提示词，不进注记。
func TestDiagnoseDocumentConfigDroppedOffAnthropic(t *testing.T) {
	yes := true
	req := &ir.Request{Model: "m", MaxTokens: 10, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "read"}, docBlock("annual report", &yes)}},
	}}
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolGemini} {
		oc, _ := codec.Outbound(name)
		notes := codec.DescribeLossy(req, name, oc.Caps())
		got := strings.Join(notes, "; ")
		if !strings.Contains(got, "per-document configuration") {
			t.Errorf("%s 应报文档配置丢失：%q", name, got)
		}
		if strings.Contains(got, "annual report") {
			t.Errorf("%s 注记带出了 context 内容：%q", name, got)
		}
	}
	oc, _ := codec.Outbound(codec.ProtocolAnthropic)
	if notes := codec.DescribeLossy(req, codec.ProtocolAnthropic, oc.Caps()); len(notes) != 0 {
		t.Errorf("anthropic 自家接得住，误报：%v", notes)
	}
}

// tool_result 内嵌的文档配置同样要数到：漏掉这层会让「工具返回了带引用开关的
// PDF」这类丢失完全不可见。
func TestDiagnoseDocumentConfigInsideToolResult(t *testing.T) {
	yes := true
	req := &ir.Request{Model: "m", MaxTokens: 10, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{
			Type: ir.BlockToolResult,
			ToolResult: &ir.ToolResult{
				ToolUseID: "tu_1",
				Content:   []ir.Block{docBlock("", &yes)},
			},
		}}},
	}}
	oc, _ := codec.Outbound(codec.ProtocolChatCompletions)
	notes := codec.DescribeLossy(req, codec.ProtocolChatCompletions, oc.Caps())
	if got := strings.Join(notes, "; "); !strings.Contains(got, "citation switch on 1 document(s)") {
		t.Errorf("内嵌文档的引用开关没数到：%q", got)
	}
}

// 响应侧：模型产出带配置的文档，外族 EncodeResponseLossy 报出，anthropic 静默。
func TestResponseDocumentConfigNotes(t *testing.T) {
	yes := true
	resp := &ir.Response{ID: "m", Model: "m", StopReason: ir.StopEndTurn,
		Content: []ir.Block{{Type: ir.BlockText, Text: "here"}, docBlock("summary", &yes)}}
	for _, name := range foreignInbound {
		notes := responseLossyNotes(t, name, resp)
		if got := strings.Join(notes, "; "); !strings.Contains(got, "per-document configuration") {
			t.Errorf("%s 应报文档配置丢失：%q", name, got)
		}
	}
	if notes := responseLossyNotes(t, codec.ProtocolAnthropic, resp); len(notes) != 0 {
		t.Errorf("anthropic 误报：%v", notes)
	}
}

// 注记按非零的那部分渲染，且两个入参方向不同措辞不同（合成一个数字会分不清
// 丢的是用途旁注还是引用开关）。
func TestDocumentConfigDropNoteShapes(t *testing.T) {
	if codec.DocumentConfigDropNote(2, 1) == codec.DocumentConfigDropNote(1, 2) {
		t.Error("注记对 (ctx,cites) 应不对称")
	}
	if s := codec.DocumentConfigDropNote(1, 0); !strings.Contains(s, "usage context") || strings.Contains(s, "citation switch") {
		t.Errorf("只带 context 的措辞不对：%s", s)
	}
	if s := codec.DocumentConfigDropNote(0, 1); !strings.Contains(s, "citation switch") || strings.Contains(s, "usage context") {
		t.Errorf("只带引用的措辞不对：%s", s)
	}
	if s := codec.DocumentConfigDropNote(1, 1); !strings.Contains(s, "usage context") || !strings.Contains(s, "citation switch") {
		t.Errorf("两者都带的措辞不对：%s", s)
	}
}

// DocConfigOf 三态：nil Media → (0,0)；context 空且 citations 缺失 → (0,0)；
// 各自非零 → 各计 1。
func TestDocConfigOf(t *testing.T) {
	if c, s := codec.DocConfigOf(nil); c != 0 || s != 0 {
		t.Errorf("nil media = (%d,%d)", c, s)
	}
	if c, s := codec.DocConfigOf(&ir.Media{}); c != 0 || s != 0 {
		t.Errorf("empty media = (%d,%d)", c, s)
	}
	no := false
	if c, s := codec.DocConfigOf(&ir.Media{Context: "x", CitationsEnabled: &no}); c != 1 || s != 1 {
		t.Errorf("显式 false 也应计 cites=1，got (%d,%d)", c, s)
	}
}
