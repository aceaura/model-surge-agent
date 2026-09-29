package codec_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	_ "github.com/aceaura/model-surge-agent/backend/codec/anthropic"
	_ "github.com/aceaura/model-surge-agent/backend/codec/chatcompletions"
	_ "github.com/aceaura/model-surge-agent/backend/codec/gemini"
	_ "github.com/aceaura/model-surge-agent/backend/codec/responses"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次74：一次丢弃只应浮出一条注记（规则 a）。此前三处子维注记不判「宿主是否已被
// 整块丢弃并报过」，与整块注记叠报同一次丢弃：
//   ① server_tool_use.caller 专项注记 —— 整块已由 ServerToolDropNote 报出；
//   ② image transformations.oversized_image —— 图片整块没送达时仍报渲染指令丢弃；
//   ④ image detail —— file_id 无从投递时仍报「上游会按默认档切图、计费不同」（假话）。
// 三者同一根因，本轮统一以「宿主是否真投递」为门。③（文档配置随本体丢弃仍单报）
// 经核实是 R70 有意设计（本体与配置两维独立），不在本轮改动，见 TestR74DocConfigStillCoexists。

func r74lossy(t *testing.T, name string, req *ir.Request) []string {
	t.Helper()
	oc, ok := codec.Outbound(name)
	if !ok {
		t.Fatalf("Outbound(%s) 未注册", name)
	}
	return codec.DescribeLossy(req, name, oc.Caps())
}

func r74count(notes []string, sub string) int {
	n := 0
	for _, s := range notes {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

func r74imageReq(m *ir.Media) *ir.Request {
	return &ir.Request{Model: "m", MaxTokens: 10, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockImage, Media: m}}},
	}}
}

// ① server_tool_use / web_search_tool_result 跨族整块丢弃，caller 随整块消失已由
// ServerToolDropNote 统一报出：只应恰有一条注记，不再有 caller 专项注记。
func TestR74ServerToolCallerNotDoubleNoted(t *testing.T) {
	caller := json.RawMessage(`{"type":"direct"}`)
	cases := map[string]*ir.Request{
		"server_tool_use": {Model: "m", MaxTokens: 10, Messages: []ir.Message{
			{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockServerToolUse,
				ServerToolUse: &ir.ServerToolUse{ID: "srvtoolu_1", Name: "web_search", Input: `{}`, Caller: caller}}}},
		}},
		"web_search_tool_result": {Model: "m", MaxTokens: 10, Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockWebSearchToolResult,
				WebSearchToolResult: &ir.WebSearchToolResult{ToolUseID: "srvtoolu_1", Caller: caller}}}},
		}},
	}
	for label, req := range cases {
		for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolGemini} {
			notes := r74lossy(t, name, req)
			if r74count(notes, "caller") != 0 {
				t.Errorf("%s→%s 不应再单报 caller 专项注记：%v", label, name, notes)
			}
			if len(notes) != 1 {
				t.Errorf("%s→%s 应恰有一条整块丢弃注记，实得 %d：%v", label, name, len(notes), notes)
			}
		}
		// anthropic 同族原样往返，一条都不该报。
		if notes := r74lossy(t, codec.ProtocolAnthropic, req); len(notes) != 0 {
			t.Errorf("%s→anthropic 自家接得住，误报：%v", label, notes)
		}
	}
}

// ② transformations.oversized_image 只在图片确实送达时才报；整块没送达（空载荷 /
// 类型不支持降级 / file_id 无从投递）时由整块注记覆盖，不再叠报。
func TestR74TransformationsGatedOnDelivery(t *testing.T) {
	foreign := []string{codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolGemini}

	// 未投递的两种「与目标无关」形态：都不该出现 transformations 注记，
	// 但整块注记必须在（防静默丢弃）。
	undelivered := []struct {
		label    string
		media    *ir.Media
		wantNote string // 覆盖整块丢弃的那条注记关键字
	}{
		{"empty", &ir.Media{MediaType: "image/png", OversizedImage: "downsize"}, "image payload"},
		{"unsupported-type", &ir.Media{MediaType: "image/tiff", Data: "AAAA", OversizedImage: "downsize"}, "downgraded to text"},
	}
	for _, tc := range undelivered {
		for _, name := range foreign {
			notes := r74lossy(t, name, r74imageReq(tc.media))
			if r74count(notes, "image transformations") != 0 {
				t.Errorf("%s→%s 图片未投递却报了 transformations：%v", tc.label, name, notes)
			}
			if r74count(notes, tc.wantNote) == 0 {
				t.Errorf("%s→%s 缺少覆盖整块丢弃的注记 %q：%v", tc.label, name, tc.wantNote, notes)
			}
		}
	}

	// file_id-only 图片：能否投递取决于目标的 ImageFileRef（responses 真、chat/gemini 假）。
	// 投得下 → transformations 照报；投不下 → 只报整块「image file reference」，不叠 transformations。
	for _, name := range foreign {
		oc, _ := codec.Outbound(name)
		notes := r74lossy(t, name, r74imageReq(&ir.Media{FileID: "file-x", OversizedImage: "downsize"}))
		if oc.Caps().ImageFileRef {
			if r74count(notes, "image transformations") != 1 {
				t.Errorf("fileref→%s（ImageFileRef 真，图片可投递）应报 transformations：%v", name, notes)
			}
		} else {
			if r74count(notes, "image transformations") != 0 {
				t.Errorf("fileref→%s（无从投递）却报了 transformations：%v", name, notes)
			}
			if r74count(notes, "image file reference") != 1 {
				t.Errorf("fileref→%s 应报整块 file reference 丢弃：%v", name, notes)
			}
		}
	}

	// 已投递（内联字节、类型被接受）：transformations 注记必须照常报出。
	delivered := &ir.Media{MediaType: "image/png", Data: "AAAA", OversizedImage: "downsize"}
	for _, name := range foreign {
		notes := r74lossy(t, name, r74imageReq(delivered))
		if r74count(notes, "image transformations") != 1 {
			t.Errorf("delivered→%s 应恰报一条 transformations，实得：%v", name, notes)
		}
	}
}

// ④ image detail 只在图片确实送达时才报；file_id 无从投递时不再叠报（那条措辞断言
// 「上游会切图、计费不同」，对没送达的图是假话）。anthropic 收 file 引用（ImageFileRef
// 真），file_id 图片可投递，detail 仍应报——与 TestForeignImageFileRefDeliveredDetailStillNoted 一致。
func TestR74DetailGatedOnDelivery(t *testing.T) {
	// gemini：ImageDetail 假、ImageFileRef 假。file_id-only 图片无从投递 → 不报 detail，
	// 但整块「image file reference」必须在。
	geminiRef := r74lossy(t, codec.ProtocolGemini, r74imageReq(&ir.Media{FileID: "file-x", Detail: "high"}))
	if r74count(geminiRef, "image detail") != 0 {
		t.Errorf("gemini file_id 图片无从投递却报了 detail：%v", geminiRef)
	}
	if r74count(geminiRef, "image file reference") != 1 {
		t.Errorf("gemini 应报整块 file reference 丢弃：%v", geminiRef)
	}

	// gemini：空载荷图片 → 不报 detail，只报 image payload。
	geminiEmpty := r74lossy(t, codec.ProtocolGemini, r74imageReq(&ir.Media{MediaType: "image/png", Detail: "high"}))
	if r74count(geminiEmpty, "image detail") != 0 {
		t.Errorf("gemini 空图片却报了 detail：%v", geminiEmpty)
	}
	if r74count(geminiEmpty, "image payload") != 1 {
		t.Errorf("gemini 应报空载荷整块丢弃：%v", geminiEmpty)
	}

	// gemini：内联字节图片可投递 → detail 照常报（唯一一条）。
	geminiData := r74lossy(t, codec.ProtocolGemini, r74imageReq(&ir.Media{MediaType: "image/png", Data: "AAAA", Detail: "high"}))
	if r74count(geminiData, "image detail") != 1 {
		t.Errorf("gemini 内联图片应报 detail 丢弃：%v", geminiData)
	}

	// anthropic：ImageFileRef 真，file_id 图片可投递；ImageDetail 假 → detail 仍报。
	antRef := r74lossy(t, codec.ProtocolAnthropic, r74imageReq(&ir.Media{FileID: "file-x", Detail: "high"}))
	if r74count(antRef, "image detail") != 1 {
		t.Errorf("anthropic file_id 图片可投递，detail 应照常报：%v", antRef)
	}
	if r74count(antRef, "image file reference") != 0 {
		t.Errorf("anthropic 原生收 file 引用，不该报 file reference 丢弃：%v", antRef)
	}
}

// ③ 反证：文档本体被整块丢弃时，配置注记（context / citations.enabled）仍单报一条——
// 这是 R70 的有意设计（本体与配置是两维，措辞各自独立、互不重复计数），本轮不改。
// 钉住它以防「一致性」名义下被误删成静默丢弃。
func TestR74DocConfigStillCoexists(t *testing.T) {
	tr := true
	req := &ir.Request{Model: "m", MaxTokens: 10, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockDocument,
			Media: &ir.Media{Context: "usage guidance", CitationsEnabled: &tr}}}},
	}}
	notes := r74lossy(t, codec.ProtocolGemini, req)
	if r74count(notes, "document blocks") == 0 {
		t.Errorf("文档本体丢弃应报出：%v", notes)
	}
	if r74count(notes, "usage context") == 0 || r74count(notes, "citation switch") == 0 {
		t.Errorf("文档配置维度应独立报出（R70 设计）：%v", notes)
	}
}
