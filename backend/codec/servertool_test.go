package codec_test

import (
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
