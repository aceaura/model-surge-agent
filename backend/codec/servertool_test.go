package codec_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 服务端托管工具块（上游自己执行的搜索）在 chat_completions / responses 里
// 没有对应形态，必须整块消失而不是退化成正文：查询 JSON 经 EvToolInput 续传，
// 不挡住就会拼进 output_text / content，客户端在回答里看到一段凭空的参数串。
// anthropic 是这两种块的原生形态，必须原样保留。
//
// 对应旧仓 #3（7025216）。web_search_call 托管 item 映射是 #70 的事，不在此。

const stQuery = `{"query":"weather in Paris"}`

// serverToolStream 服务端工具的事件形态：server_tool_use(+查询串增量) /
// web_search_tool_result / 摘要文本。
func serverToolStream() []ir.Event {
	return []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "msg", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
			Type:          ir.BlockServerToolUse,
			ServerToolUse: &ir.ServerToolUse{ID: "srvtoolu_1", Name: "web_search"},
		}},
		{Type: ir.EvToolInput, Index: 0, Text: stQuery},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvBlockStart, Index: 1, Block: &ir.Block{
			Type: ir.BlockWebSearchToolResult,
			WebSearchToolResult: &ir.WebSearchToolResult{
				ToolUseID: "srvtoolu_1",
				Results:   []ir.WebSearchResult{{Title: "Paris weather", URL: "https://example.com/p"}},
			},
		}},
		{Type: ir.EvBlockStop, Index: 1},
		{Type: ir.EvBlockStart, Index: 2, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 2, Text: "It is sunny."},
		{Type: ir.EvBlockStop, Index: 2},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Usage: &ir.Usage{OutputTokens: 3}},
		{Type: ir.EvMessageStop},
	}
}

// foreignInbound 入站两外族（anthropic 是原生形态；gemini 只有出站，不作客户端）。
var foreignInbound = []string{codec.ProtocolChatCompletions, codec.ProtocolResponses}

func TestServerToolBlocksNotLeakedIntoStream(t *testing.T) {
	for _, name := range foreignInbound {
		t.Run(name, func(t *testing.T) {
			out := renderStream(t, name, serverToolStream())
			if strings.Contains(out, "weather in Paris") {
				t.Errorf("服务端工具查询参数泄漏进流：\n%s", out)
			}
			if strings.Contains(out, "srvtoolu_1") {
				t.Errorf("服务端工具 id 泄漏：\n%s", out)
			}
			if !strings.Contains(out, "It is sunny.") {
				t.Errorf("摘要正文被误删：\n%s", out)
			}
		})
	}
}

// Anthropic 是这两种块的原生形态，必须原样保留——「不泄漏」只适用于装不下它们的协议。
func TestServerToolBlocksPreservedForAnthropic(t *testing.T) {
	out := renderStream(t, codec.ProtocolAnthropic, serverToolStream())
	for _, want := range []string{"server_tool_use", "web_search_tool_result", "srvtoolu_1", "weather in Paris"} {
		if !strings.Contains(out, want) {
			t.Errorf("anthropic 丢了原生块内容 %q：\n%s", want, out)
		}
	}
}

// 非流式方向：两外族不输出对应形态，不得泄漏；摘要正文保留。
func TestServerToolBlocksNotLeakedIntoResponse(t *testing.T) {
	resp := &ir.Response{
		ID: "msg", Model: "m", StopReason: ir.StopEndTurn,
		Content: []ir.Block{
			{Type: ir.BlockServerToolUse, ServerToolUse: &ir.ServerToolUse{
				ID: "srvtoolu_1", Name: "web_search", Input: stQuery,
			}},
			{Type: ir.BlockWebSearchToolResult, WebSearchToolResult: &ir.WebSearchToolResult{ToolUseID: "srvtoolu_1"}},
			{Type: ir.BlockText, Text: "It is sunny."},
		},
	}
	for _, name := range foreignInbound {
		t.Run(name, func(t *testing.T) {
			c, ok := codec.Inbound(name)
			if !ok {
				t.Fatalf("inbound %q not registered", name)
			}
			body, err := c.EncodeResponse(resp)
			if err != nil {
				t.Fatalf("EncodeResponse: %v", err)
			}
			if strings.Contains(string(body), "weather in Paris") || strings.Contains(string(body), "srvtoolu_1") {
				t.Errorf("非流式响应泄漏服务端工具块：%s", body)
			}
			if !strings.Contains(string(body), "It is sunny.") {
				t.Errorf("摘要正文被误删：%s", body)
			}
		})
	}
}

// responses 跳过服务端工具块不得烧掉 output_index：真块的序号必须从 0 连续。
func TestResponsesOutputIndexStaysDenseAfterSkip(t *testing.T) {
	out := renderStream(t, codec.ProtocolResponses, serverToolStream())
	// 服务端工具块占 IR 索引 0/1，正文块占 IR 索引 2；跳过后正文的 output_index 应是 0。
	if strings.Contains(out, `"output_index":1`) || strings.Contains(out, `"output_index":2`) {
		t.Errorf("output_index 被跳过的块烧掉了序号：\n%s", out)
	}
	if !strings.Contains(out, `"output_index":0`) {
		t.Errorf("正文块没有拿到 output_index 0：\n%s", out)
	}
}

// 跳过服务端工具块不能顺手吞掉普通工具调用的参数：二者都走 EvToolInput，
// 只有前者在 skip 集合里。
func TestRegularToolArgumentsStillReachWire(t *testing.T) {
	events := []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "msg", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
			Type:    ir.BlockToolUse,
			ToolUse: &ir.ToolUse{ID: "toolu_1", Name: "get_weather"},
		}},
		{Type: ir.EvToolInput, Index: 0, Text: `{"city":"Paris"}`},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvMessageDelta, StopReason: ir.StopToolUse},
		{Type: ir.EvMessageStop},
	}
	for _, name := range foreignInbound {
		t.Run(name, func(t *testing.T) {
			out := renderStream(t, name, events)
			if !strings.Contains(out, "Paris") {
				t.Errorf("普通工具调用参数被误吞：\n%s", out)
			}
			if !strings.Contains(out, "get_weather") {
				t.Errorf("普通工具名丢失：\n%s", out)
			}
		})
	}
}

// 请求方向：多轮历史里带服务端工具块（anthropic 客户端回传上一轮的搜索痕迹）
// 投给外族上游时整块跳过，既不硬报错也不泄漏。
func TestServerToolBlocksSkippedInForeignRequest(t *testing.T) {
	req := &ir.Request{
		Model: "m",
		Messages: []ir.Message{{
			Role: ir.RoleAssistant,
			Content: []ir.Block{
				{Type: ir.BlockServerToolUse, ServerToolUse: &ir.ServerToolUse{
					ID: "srvtoolu_1", Name: "web_search", Input: stQuery,
				}},
				{Type: ir.BlockWebSearchToolResult, WebSearchToolResult: &ir.WebSearchToolResult{ToolUseID: "srvtoolu_1"}},
				{Type: ir.BlockText, Text: "It is sunny."},
			},
		}},
	}
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolGemini} {
		t.Run(name, func(t *testing.T) {
			c, ok := codec.Outbound(name)
			if !ok {
				t.Fatalf("outbound %q not registered", name)
			}
			body, err := c.EncodeRequest(req)
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			if strings.Contains(string(body), "weather in Paris") || strings.Contains(string(body), "srvtoolu_1") {
				t.Errorf("请求体泄漏服务端工具块：%s", body)
			}
			if !strings.Contains(string(body), "It is sunny.") {
				t.Errorf("摘要正文被误删：%s", body)
			}
		})
	}
}

// ---- #61：不泄漏只是一半，丢了还必须报出来 ----
//
// 上面那批测试钉的是「块体不进正文」。但外族在流式、非流式与请求侧三条
// 路径上都曾经零注记：调用方拿到的响应看不出这一轮少了什么，模型也不知道
// 自己上一轮搜过什么。以下把可见性钉住。

// serverToolOnly 只有托管工具块（2 个调用 + 1 个结果）。计数刻意不对称，
// 好让「两个计数接反」这类错误被测出来。
func serverToolOnly() []ir.Block {
	return []ir.Block{
		{Type: ir.BlockServerToolUse, ServerToolUse: &ir.ServerToolUse{
			ID: "srvtoolu_1", Name: "web_search", Input: stQuery,
		}},
		{Type: ir.BlockServerToolUse, ServerToolUse: &ir.ServerToolUse{
			ID: "srvtoolu_2", Name: "web_search", Input: `{"query":"weather in Lyon"}`,
		}},
		{Type: ir.BlockWebSearchToolResult, WebSearchToolResult: &ir.WebSearchToolResult{
			ToolUseID: "srvtoolu_1",
			Results: []ir.WebSearchResult{
				{Title: "Paris weather", URL: "https://example.com/p", Snippet: "sunny"},
			},
		}},
	}
}

// serverToolResp 托管工具块夹在正文中间：既报损耗，也不许动正文。
func serverToolResp() *ir.Response {
	blocks := append([]ir.Block{{Type: ir.BlockText, Text: "lead"}}, serverToolOnly()...)
	blocks = append(blocks, ir.Block{Type: ir.BlockText, Text: "It is sunny."})
	return &ir.Response{ID: "m1", Model: "m", StopReason: ir.StopEndTurn, Content: blocks}
}

// blockEvents 把块序列摊成最小事件流（每块只有 start/stop，无 delta）。
func blockEvents(blocks []ir.Block) []ir.Event {
	evs := []ir.Event{{Type: ir.EvMessageStart, MessageID: "msg", Model: "m"}}
	for i := range blocks {
		b := blocks[i]
		evs = append(evs,
			ir.Event{Type: ir.EvBlockStart, Index: i, Block: &b},
			ir.Event{Type: ir.EvBlockStop, Index: i})
	}
	return append(evs,
		ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn},
		ir.Event{Type: ir.EvMessageStop})
}

// renderStreamNotes 同 renderStream，另返回编码器交出的损耗注记。
func renderStreamNotes(t *testing.T, protocol string, events []ir.Event) (string, []string) {
	t.Helper()
	c, ok := codec.Inbound(protocol)
	if !ok {
		t.Fatalf("inbound %q not registered", protocol)
	}
	enc := c.NewStreamEncoder(nil)
	var b strings.Builder
	for _, ev := range events {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("%s encode %s: %v", protocol, ev.Type, err)
		}
		for _, f := range frames {
			b.Write(f)
		}
	}
	for _, f := range enc.Finish() {
		b.Write(f)
	}
	var notes []string
	if n, ok := enc.(codec.StreamNotes); ok {
		notes = n.Notes()
	}
	return b.String(), notes
}

// assertNoSessionContent 注记里不得出现搜索结果的标题/URL/摘要或查询参数：
// 那是会话内容，注记会进响应头与日志。
func assertNoSessionContent(t *testing.T, notes []string) {
	t.Helper()
	for _, n := range notes {
		for _, leak := range []string{"example.com", "Paris weather", "sunny",
			"weather in Paris", "weather in Lyon"} {
			if strings.Contains(n, leak) {
				t.Errorf("注记带出了会话内容 %q：%s", leak, n)
			}
		}
	}
}

func TestServerToolDropReportedInStreamNotes(t *testing.T) {
	if codec.ServerToolDropNote(2, 1) == codec.ServerToolDropNote(1, 2) {
		t.Fatal("注记措辞对两个计数对称，测不出计数接反")
	}
	want := codec.ServerToolDropNote(2, 1)
	for _, name := range foreignInbound {
		t.Run(name, func(t *testing.T) {
			_, notes := renderStreamNotes(t, name, blockEvents(serverToolOnly()))
			if !slices.Contains(notes, want) {
				t.Fatalf("流式未报托管工具块损耗：notes=%q", notes)
			}
			assertNoSessionContent(t, notes)
		})
	}
}

func TestServerToolDropReportedInResponseNotes(t *testing.T) {
	want := codec.ServerToolDropNote(2, 1)
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
			body, notes, err := lossy.EncodeResponseLossy(serverToolResp())
			if err != nil {
				t.Fatalf("EncodeResponseLossy: %v", err)
			}
			if !slices.Contains(notes, want) {
				t.Fatalf("非流式扫描未报托管工具块损耗：notes=%q", notes)
			}
			assertNoSessionContent(t, notes)
			for _, keep := range []string{"lead", "It is sunny."} {
				if !strings.Contains(string(body), keep) {
					t.Errorf("报了损耗却顺手删了正文 %q：%s", keep, body)
				}
			}
		})
	}
}

// 只有调用没有结果（上游断在两者之间）时也得报，且计数只带非零那半。
func TestServerToolDropReportedForCallsOnly(t *testing.T) {
	blocks := serverToolOnly()[:2]
	want := codec.ServerToolDropNote(2, 0)
	for _, name := range foreignInbound {
		t.Run(name, func(t *testing.T) {
			_, notes := renderStreamNotes(t, name, blockEvents(blocks))
			if !slices.Contains(notes, want) {
				t.Fatalf("只有托管调用时未报损耗：notes=%q", notes)
			}
		})
	}
}

// anthropic 是原生形态：块要照常输出，注记必须闭嘴——否则等于谎报损耗。
func TestServerToolDropSilentForAnthropic(t *testing.T) {
	c, ok := codec.Inbound(codec.ProtocolAnthropic)
	if !ok {
		t.Fatal("inbound anthropic not registered")
	}
	lossy, ok := c.(codec.LossyResponseEncoder)
	if !ok {
		t.Fatal("anthropic 不实现 LossyResponseEncoder")
	}
	_, respNotes, err := lossy.EncodeResponseLossy(serverToolResp())
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	_, streamNotes := renderStreamNotes(t, codec.ProtocolAnthropic, blockEvents(serverToolOnly()))
	for _, notes := range [][]string{respNotes, streamNotes} {
		for _, n := range notes {
			if strings.Contains(n, "server-side tool") || strings.Contains(n, "web search result") {
				t.Errorf("anthropic 谎报托管工具块损耗：%s", n)
			}
		}
	}
}

// 注记主语按计数分三种形态；两种块型都为零时不该被调用方拿来生成注记，
// 这里只钉措辞本身。
func TestServerToolDropNoteSubjectShapes(t *testing.T) {
	for _, tc := range []struct {
		calls, results int
		prefix         string
	}{
		{1, 0, "dropped 1 server-side tool call(s):"},
		{0, 1, "dropped 1 web search result block(s):"},
		{2, 3, "dropped 2 server-side tool call(s) and 3 web search result block(s):"},
	} {
		got := codec.ServerToolDropNote(tc.calls, tc.results)
		if !strings.HasPrefix(got, tc.prefix) {
			t.Errorf("ServerToolDropNote(%d,%d) 主语不对：\n got %s\nwant 前缀 %s",
				tc.calls, tc.results, got, tc.prefix)
		}
	}
}

// 请求侧：历史里的托管工具块投给外族上游时整块消失，诊断必须报出；
// 报损耗的前提是真的丢了——请求体里不该再有痕迹。
func TestServerToolDropReportedInRequestNotes(t *testing.T) {
	req := &ir.Request{
		Model: "m", MaxTokens: 64,
		Messages: []ir.Message{{Role: ir.RoleAssistant, Content: serverToolOnly()}},
	}
	want := codec.ServerToolDropNote(2, 1)
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
			body, notes, err := lossy.EncodeRequestLossy(req)
			if err != nil {
				t.Fatalf("EncodeRequestLossy: %v", err)
			}
			if !slices.Contains(notes, want) {
				t.Fatalf("请求侧未报托管工具块损耗：notes=%q", notes)
			}
			assertNoSessionContent(t, notes)
			for _, gone := range []string{"srvtoolu_1", "srvtoolu_2", "example.com",
				"weather in Paris", "server_tool_use"} {
				if strings.Contains(string(body), gone) {
					t.Errorf("谎报损耗：请求体里仍有 %q：%s", gone, body)
				}
			}
		})
	}
}

// anthropic 出站原样往返：不报注记，块体照发。
func TestServerToolDropSilentForAnthropicRequest(t *testing.T) {
	req := &ir.Request{
		Model: "m", MaxTokens: 64,
		Messages: []ir.Message{{Role: ir.RoleAssistant, Content: serverToolOnly()}},
	}
	c, ok := codec.Outbound(codec.ProtocolAnthropic)
	if !ok {
		t.Fatal("outbound anthropic not registered")
	}
	lossy, ok := c.(codec.LossyEncoder)
	if !ok {
		t.Fatal("anthropic 不实现 LossyEncoder")
	}
	body, notes, err := lossy.EncodeRequestLossy(req)
	if err != nil {
		t.Fatalf("EncodeRequestLossy: %v", err)
	}
	for _, n := range notes {
		if strings.Contains(n, "server-side tool") || strings.Contains(n, "web search result") {
			t.Errorf("anthropic 谎报托管工具块损耗：%s", n)
		}
	}
	for _, keep := range []string{"server_tool_use", "web_search_tool_result", "srvtoolu_1"} {
		if !strings.Contains(string(body), keep) {
			t.Errorf("anthropic 出站丢了原生块 %q：%s", keep, body)
		}
	}
}
