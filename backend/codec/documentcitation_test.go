package codec_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	_ "github.com/aceaura/model-surge-agent/backend/codec/anthropic"
	_ "github.com/aceaura/model-surge-agent/backend/codec/chatcompletions"
	_ "github.com/aceaura/model-surge-agent/backend/codec/gemini"
	_ "github.com/aceaura/model-surge-agent/backend/codec/responses"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// Anthropic 的 citations 是五形态判别 union：文档类三条（char_location /
// page_location / content_block_location）靠 document_index 与页/块/字符
// 下标定位，没有 URL；search_result_location 有 source；只有
// web_search_result_location 有 url。chat / responses 的标注槽位以 URL 为
// 来源身份，文档类装不下，必须干净丢弃并走三通道注记；anthropic 同族往返
// 走 Raw 原样带回，一条不丢也不许报损耗。
//
// 对应旧仓 #59（c08951e）。gemini 没有标注槽位，整族丢弃的口径由
// citationlossy_test.go 钉住，这里只钉「有槽位但逐条装不下」的外族。

// 官方文档类引用原文，字段集取自 Anthropic 文档，Raw 往返的字节基准。
const (
	charLocRaw = `{"type":"char_location","cited_text":"晴","document_index":0,"document_title":"天气报告","start_char_index":4,"end_char_index":5,"file_id":"file_abc"}`
	pageLocRaw = `{"type":"page_location","cited_text":"晴","document_index":0,"start_page_number":2,"end_page_number":3}`
	searchRaw  = `{"type":"search_result_location","cited_text":"晴","search_result_index":2,"source":"https://s","title":"搜索结果"}`
)

// docCiteLeaks 是外族输出（正文帧、响应体、请求体）与注记里都不得出现的
// 文档类引用痕迹：出现即说明定位字段被硬塞进了 URL 槽位或正文。
var docCiteLeaks = []string{
	"char_location", "page_location", "document_index",
	"file_abc", "start_page_number", "天气报告",
}

// docCitations 两条不可移植（文档类，带 Raw）+ 一条可移植（托管搜索，
// source 已投影成 URL）。计数刻意不对称：全不可移植的夹具测不出
// 「Portable 判据退化成恒假」这类错误。
func docCitations() []ir.Citation {
	return []ir.Citation{
		{WireType: "char_location", CitedText: "晴", Title: "天气报告",
			Start: 4, End: 5, Raw: json.RawMessage(charLocRaw)},
		{WireType: "page_location", CitedText: "晴",
			Raw: json.RawMessage(pageLocRaw)},
		{WireType: "search_result_location", URL: "https://s", Title: "搜索结果",
			CitedText: "晴", Raw: json.RawMessage(searchRaw)},
	}
}

const docCiteText = "北京今天晴"

// docCiteStream 正文增量之后跟一帧引用增量：客户端按增量拼正文，
// 引用帧到达时正文已在它手里。
func docCiteStream() []ir.Event {
	return []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "msg", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 0, Text: docCiteText},
		{Type: ir.EvCitation, Index: 0, Citations: docCitations()},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Usage: &ir.Usage{OutputTokens: 3}},
		{Type: ir.EvMessageStop},
	}
}

func docCiteResp() *ir.Response {
	return &ir.Response{
		ID: "m1", Model: "m", StopReason: ir.StopEndTurn,
		Content: []ir.Block{{Type: ir.BlockText, Text: docCiteText, Citations: docCitations()}},
	}
}

func docCiteReq() *ir.Request {
	return &ir.Request{
		Model: "m", MaxTokens: 64,
		Messages: []ir.Message{{
			Role:    ir.RoleAssistant,
			Content: []ir.Block{{Type: ir.BlockText, Text: docCiteText, Citations: docCitations()}},
		}},
	}
}

// assertNoDocCiteLeaks 外族输出里不得残留文档类引用的定位字段。
func assertNoDocCiteLeaks(t *testing.T, what, out string) {
	t.Helper()
	for _, leak := range docCiteLeaks {
		if strings.Contains(out, leak) {
			t.Errorf("%s泄漏文档类引用痕迹 %q：\n%s", what, leak, out)
		}
	}
}

// assertNotesNoSessionContent 注记会进响应头与日志，不得带出引用的
// 原文、标题或文件名。
func assertNotesNoSessionContent(t *testing.T, notes []string) {
	t.Helper()
	joined := strings.Join(notes, "\n")
	assertNoDocCiteLeaks(t, "注记", joined)
	if strings.Contains(joined, "晴") || strings.Contains(joined, "https://s") {
		t.Errorf("注记带出了会话内容：%s", joined)
	}
}

// ---- 流式通道 ----

func TestDocCitationsNotLeakedIntoForeignStream(t *testing.T) {
	want := codec.CitationDropNote(2)
	for _, name := range foreignInbound {
		t.Run(name, func(t *testing.T) {
			out, notes := renderStreamNotes(t, name, docCiteStream())
			assertNoDocCiteLeaks(t, "流式输出", out)
			if !strings.Contains(out, docCiteText) {
				t.Errorf("正文被误删：\n%s", out)
			}
			if !slices.Contains(notes, want) {
				t.Fatalf("流式未报文档类引用损耗：notes=%q", notes)
			}
			assertNotesNoSessionContent(t, notes)
		})
	}
}

// anthropic 是文档类引用的原生形态：Raw 原样进帧，注记闭嘴。
func TestDocCitationsPreservedForAnthropicStream(t *testing.T) {
	out, notes := renderStreamNotes(t, codec.ProtocolAnthropic, docCiteStream())
	for _, keep := range []string{"char_location", "document_index", "file_abc", "天气报告", "start_page_number"} {
		if !strings.Contains(out, keep) {
			t.Errorf("anthropic 流式丢了原文字段 %q：\n%s", keep, out)
		}
	}
	for _, n := range notes {
		if strings.Contains(n, "citation(s)") {
			t.Errorf("anthropic 谎报引用损耗：%s", n)
		}
	}
}

// ---- 非流式响应通道 ----

func TestDocCitationsNotLeakedIntoForeignResponse(t *testing.T) {
	want := codec.CitationDropNote(2)
	for _, name := range foreignInbound {
		t.Run(name, func(t *testing.T) {
			c, ok := codec.Inbound(name)
			if !ok {
				t.Fatalf("inbound %q not registered", name)
			}
			lossy, ok := c.(codec.LossyResponseEncoder)
			if !ok {
				t.Fatalf("%s 不实现 LossyResponseEncoder", name)
			}
			body, notes, err := lossy.EncodeResponseLossy(docCiteResp())
			if err != nil {
				t.Fatalf("EncodeResponseLossy: %v", err)
			}
			assertNoDocCiteLeaks(t, "响应体", string(body))
			if strings.Contains(string(body), `"url":""`) {
				t.Errorf("文档类引用被塞进空 URL 槽位：%s", body)
			}
			if !strings.Contains(string(body), docCiteText) {
				t.Errorf("正文被误删：%s", body)
			}
			if !slices.Contains(notes, want) {
				t.Fatalf("非流式扫描未报文档类引用损耗：notes=%q", notes)
			}
			assertNotesNoSessionContent(t, notes)
		})
	}
}

// 全可移植（纯 URL 引用）与无引用两种响应，三入站一律不出注记：
// 恒定出现的注记会淹没真正丢了东西的那几条。
func TestDocCitationResponseNotesSilentWithoutDocumentCitations(t *testing.T) {
	resps := map[string]*ir.Response{
		"纯URL引用": {ID: "m1", Model: "m", StopReason: ir.StopEndTurn,
			Content: []ir.Block{{Type: ir.BlockText, Text: docCiteText, Citations: []ir.Citation{
				{URL: "https://w", Title: "T", Start: 0, End: 2},
			}}}},
		"无引用": {ID: "m1", Model: "m", StopReason: ir.StopEndTurn,
			Content: []ir.Block{{Type: ir.BlockText, Text: docCiteText}}},
	}
	for _, name := range append([]string{codec.ProtocolAnthropic}, foreignInbound...) {
		c, ok := codec.Inbound(name)
		if !ok {
			t.Fatalf("inbound %q not registered", name)
		}
		lossy, ok := c.(codec.LossyResponseEncoder)
		if !ok {
			t.Fatalf("%s 不实现 LossyResponseEncoder", name)
		}
		for caseName, resp := range resps {
			t.Run(name+"/"+caseName, func(t *testing.T) {
				_, notes, err := lossy.EncodeResponseLossy(resp)
				if err != nil {
					t.Fatalf("EncodeResponseLossy: %v", err)
				}
				for _, n := range notes {
					if strings.Contains(n, "citation(s)") {
						t.Errorf("没有文档类引用却报了损耗：%s", n)
					}
				}
			})
		}
	}
}

// anthropic 非流式：五形态经 Raw 原样回吐，注记闭嘴。
func TestDocCitationsPreservedForAnthropicResponse(t *testing.T) {
	c, ok := codec.Inbound(codec.ProtocolAnthropic)
	if !ok {
		t.Fatal("inbound anthropic not registered")
	}
	lossy, ok := c.(codec.LossyResponseEncoder)
	if !ok {
		t.Fatal("anthropic 不实现 LossyResponseEncoder")
	}
	body, notes, err := lossy.EncodeResponseLossy(docCiteResp())
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	for _, keep := range []string{"char_location", "document_index", "file_abc", "天气报告"} {
		if !strings.Contains(string(body), keep) {
			t.Errorf("anthropic 响应丢了原文字段 %q：%s", keep, body)
		}
	}
	for _, n := range notes {
		if strings.Contains(n, "citation(s)") {
			t.Errorf("anthropic 谎报引用损耗：%s", n)
		}
	}
}

// ---- 出站请求通道 ----

func TestDocCitationsNotLeakedIntoForeignRequest(t *testing.T) {
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolGemini} {
		t.Run(name, func(t *testing.T) {
			c, ok := codec.Outbound(name)
			if !ok {
				t.Fatalf("outbound %q not registered", name)
			}
			lossy, ok := c.(codec.LossyEncoder)
			if !ok {
				t.Fatalf("%s 不实现 LossyEncoder", name)
			}
			body, notes, err := lossy.EncodeRequestLossy(docCiteReq())
			if err != nil {
				t.Fatalf("EncodeRequestLossy: %v", err)
			}
			assertNoDocCiteLeaks(t, "请求体", string(body))
			if strings.Contains(string(body), `"url":""`) {
				t.Errorf("文档类引用被塞进空 URL 槽位：%s", body)
			}
			if !strings.Contains(string(body), docCiteText) {
				t.Errorf("历史正文被误删：%s", body)
			}
			assertNotesNoSessionContent(t, notes)
			// gemini 没有槽位，整族三条全报；chat / responses 有槽位，
			// 只报装不下的两条。
			switch name {
			case codec.ProtocolGemini:
				if !slices.ContainsFunc(notes, func(n string) bool {
					return strings.Contains(n, "dropped 3 citation(s)")
				}) {
					t.Errorf("gemini 未报整族引用损耗：notes=%q", notes)
				}
			default:
				if !slices.Contains(notes, codec.CitationDropNote(2)) {
					t.Errorf("请求侧未报文档类引用损耗：notes=%q", notes)
				}
			}
		})
	}
}

// anthropic 出站：历史里的文档类引用原样发给上游（多轮文档问答就靠它）。
func TestDocCitationsPreservedForAnthropicRequest(t *testing.T) {
	c, ok := codec.Outbound(codec.ProtocolAnthropic)
	if !ok {
		t.Fatal("outbound anthropic not registered")
	}
	lossy, ok := c.(codec.LossyEncoder)
	if !ok {
		t.Fatal("anthropic 不实现 LossyEncoder")
	}
	body, notes, err := lossy.EncodeRequestLossy(docCiteReq())
	if err != nil {
		t.Fatalf("EncodeRequestLossy: %v", err)
	}
	for _, keep := range []string{"char_location", "document_index", "file_abc", "天气报告"} {
		if !strings.Contains(string(body), keep) {
			t.Errorf("anthropic 出站丢了引用原文字段 %q：%s", keep, body)
		}
	}
	for _, n := range notes {
		if strings.Contains(n, "citation(s)") {
			t.Errorf("anthropic 谎报引用损耗：%s", n)
		}
	}
}
