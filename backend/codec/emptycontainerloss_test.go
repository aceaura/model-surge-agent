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

// 轮次54：RawMessage 透传的请求侧参数（mcp_servers 是数组、context_management 与
// access_programs 是对象）在客户端显式给了「空容器」时，语义等于什么都没声明，跨族
// 丢弃它不损失任何信息，不该报「声明的 X 被丢弃」的假阳性注记——与轮次53 responses
// output_text.logprobs:[] 被误计为丢弃同款缺口，违反「注记当且仅当真实丢弃」不变量
// （误报与漏报同为缺口）。部分 SDK 会把「空列表」序列化成 [] 而非省略，故 [] / [ ] /
// {} / { } / null 一律视为「没给」；只有真带了内容才照实报出。

// 三族都没有 MCP 连接器 / 上下文编辑槽位（anthropic 是这俩的宿主）。
var emptyForeign = []string{codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolGemini}

// access_programs 的宿主是 responses，故其外族是另外三族。
var emptyForeignForAccess = []string{codec.ProtocolAnthropic, codec.ProtocolChatCompletions, codec.ProtocolGemini}

func emptyBaseReq() *ir.Request {
	return &ir.Request{Model: "m", MaxTokens: 10,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
}

func lossyJoined(t *testing.T, req *ir.Request, name string) string {
	t.Helper()
	oc, _ := codec.Outbound(name)
	return strings.Join(codec.DescribeLossy(req, name, oc.Caps()), "; ")
}

// 空 mcp_servers（[] / [ ] / null）跨族一律静默，不得报「声明的 MCP 服务器被丢弃」。
func TestEmptyMcpServersNotDropped(t *testing.T) {
	for _, raw := range []string{`[]`, `[ ]`, `null`} {
		req := emptyBaseReq()
		req.McpServers = json.RawMessage(raw)
		for _, name := range emptyForeign {
			if got := lossyJoined(t, req, name); strings.Contains(got, "mcp_servers") {
				t.Errorf("mcp_servers=%s 在 %s 被误报丢弃：%q", raw, name, got)
			}
		}
	}
}

// 实载荷 mcp_servers 仍须照实报出（修的是假阳性，不是把注记关掉）。
func TestPopulatedMcpServersStillDropped(t *testing.T) {
	req := emptyBaseReq()
	req.McpServers = json.RawMessage(`[{"type":"url","url":"https://x/mcp","name":"srv"}]`)
	for _, name := range emptyForeign {
		if got := lossyJoined(t, req, name); !strings.Contains(got, "no MCP connector slot") {
			t.Errorf("%s 实载荷 mcp_servers 未报丢弃：%q", name, got)
		}
	}
}

// 空 context_management（{} / { } / null）跨族一律静默。
func TestEmptyContextManagementNotDropped(t *testing.T) {
	for _, raw := range []string{`{}`, `{ }`, `null`} {
		req := emptyBaseReq()
		req.ContextManagement = json.RawMessage(raw)
		for _, name := range emptyForeign {
			if got := lossyJoined(t, req, name); strings.Contains(got, "context_management") {
				t.Errorf("context_management=%s 在 %s 被误报丢弃：%q", raw, name, got)
			}
		}
	}
}

func TestPopulatedContextManagementStillDropped(t *testing.T) {
	req := emptyBaseReq()
	req.ContextManagement = json.RawMessage(`{"edits":[{"type":"clear_tool_uses_20250919"}]}`)
	for _, name := range emptyForeign {
		if got := lossyJoined(t, req, name); !strings.Contains(got, "no context-editing slot") {
			t.Errorf("%s 实载荷 context_management 未报丢弃：%q", name, got)
		}
	}
}

// 空 access_programs（{} / { } / null）对非 responses 外族一律静默。
func TestEmptyAccessProgramsNotDropped(t *testing.T) {
	for _, raw := range []string{`{}`, `{ }`, `null`} {
		req := emptyBaseReq()
		req.AccessPrograms = json.RawMessage(raw)
		for _, name := range emptyForeignForAccess {
			if got := lossyJoined(t, req, name); strings.Contains(got, "access_programs") {
				t.Errorf("access_programs=%s 在 %s 被误报丢弃：%q", raw, name, got)
			}
		}
	}
}

func TestPopulatedAccessProgramsStillDropped(t *testing.T) {
	req := emptyBaseReq()
	req.AccessPrograms = json.RawMessage(`{"cyber":"daybreak_blue"}`)
	for _, name := range emptyForeignForAccess {
		if got := lossyJoined(t, req, name); !strings.Contains(got, "no domain-specific access-program slot") {
			t.Errorf("%s 实载荷 access_programs 未报丢弃：%q", name, got)
		}
	}
}
