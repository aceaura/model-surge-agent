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

// anthropic 两个 beta 请求参数（mcp_servers / context_management）的跨族损耗。
// 二者是 anthropic 专属维度、外族无等价槽位：请求侧走 DescribeLossy，anthropic
// 自家静默、缺席全静默、外族照实报出。同族原文透传见 anthropic/betaparams_test.go。
//
// 对应轮次22（wire 字段覆盖系统性广审）。

// betaForeign 是三个没有 MCP 连接器 / 上下文编辑槽位的出站协议。
var betaForeign = []string{codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolGemini}

func betaReq() *ir.Request {
	return &ir.Request{Model: "m", MaxTokens: 10,
		Messages:          []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		McpServers:        json.RawMessage(`[{"type":"url","url":"https://x/mcp","name":"srv"}]`),
		ContextManagement: json.RawMessage(`{"edits":[{"type":"clear_tool_uses_20250919"}]}`)}
}

func TestDiagnoseMcpServersDroppedOffAnthropic(t *testing.T) {
	req := betaReq()
	for _, name := range betaForeign {
		oc, _ := codec.Outbound(name)
		got := strings.Join(codec.DescribeLossy(req, name, oc.Caps()), "; ")
		if !strings.Contains(got, "mcp_servers") || !strings.Contains(got, "no MCP connector slot") {
			t.Errorf("%s 应报 mcp_servers 丢失：%q", name, got)
		}
	}
	// anthropic 自家接得住，静默（就 mcp_servers 而言——其余维度不在本用例范围）。
	oc, _ := codec.Outbound(codec.ProtocolAnthropic)
	for _, n := range codec.DescribeLossy(req, codec.ProtocolAnthropic, oc.Caps()) {
		if strings.Contains(n, "mcp_servers") {
			t.Errorf("anthropic 自家误报 mcp_servers：%v", n)
		}
	}
}

func TestDiagnoseContextManagementDroppedOffAnthropic(t *testing.T) {
	req := betaReq()
	for _, name := range betaForeign {
		oc, _ := codec.Outbound(name)
		got := strings.Join(codec.DescribeLossy(req, name, oc.Caps()), "; ")
		if !strings.Contains(got, "context_management") || !strings.Contains(got, "no context-editing slot") {
			t.Errorf("%s 应报 context_management 丢失：%q", name, got)
		}
	}
	oc, _ := codec.Outbound(codec.ProtocolAnthropic)
	for _, n := range codec.DescribeLossy(req, codec.ProtocolAnthropic, oc.Caps()) {
		if strings.Contains(n, "context_management") {
			t.Errorf("anthropic 自家误报 context_management：%v", n)
		}
	}
}

// 缺席（两字段都空）时全协议静默，且各自独立缺席也互不误报。
func TestDiagnoseBetaParamsAbsentSilent(t *testing.T) {
	base := &ir.Request{Model: "m", MaxTokens: 10,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
	for _, name := range codec.OutboundNames() {
		oc, _ := codec.Outbound(name)
		if notes := codec.DescribeLossy(base, name, oc.Caps()); len(notes) != 0 {
			t.Errorf("%s 无 beta 参数误报：%v", name, notes)
		}
	}
	// 只带 mcp_servers 时不应误报 context_management，反之亦然。
	onlyMcp := betaReq()
	onlyMcp.ContextManagement = nil
	onlyCtx := betaReq()
	onlyCtx.McpServers = nil
	for _, name := range betaForeign {
		oc, _ := codec.Outbound(name)
		joined := strings.Join(codec.DescribeLossy(onlyMcp, name, oc.Caps()), "; ")
		if strings.Contains(joined, "context_management") {
			t.Errorf("%s 只带 mcp_servers 却误报 context_management：%q", name, joined)
		}
		joined = strings.Join(codec.DescribeLossy(onlyCtx, name, oc.Caps()), "; ")
		if strings.Contains(joined, "mcp_servers") {
			t.Errorf("%s 只带 context_management 却误报 mcp_servers：%q", name, joined)
		}
	}
}

// 跨族出站编码不得把这两个键泄漏到外族线格式（丢弃要干净，注记负责告知）。
func TestBetaParamsNotWrittenToForeignWire(t *testing.T) {
	req := betaReq()
	for _, name := range betaForeign {
		out := string(encodeOut(t, name, req))
		if strings.Contains(out, "mcp_servers") {
			t.Errorf("%s 出站泄漏 mcp_servers：%s", name, out)
		}
		if strings.Contains(out, "context_management") {
			t.Errorf("%s 出站泄漏 context_management：%s", name, out)
		}
	}
	// 同族 anthropic 出站必须写回（透传兑现）。
	out := string(encodeOut(t, codec.ProtocolAnthropic, req))
	if !strings.Contains(out, "mcp_servers") || !strings.Contains(out, "context_management") {
		t.Errorf("anthropic 同族未回写 beta 参数：%s", out)
	}
}
