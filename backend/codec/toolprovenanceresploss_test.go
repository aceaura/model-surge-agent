package codec

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次64：上游响应里工具调用的两族发起方 provenance 标记（anthropic 族
// caller/toolset_name、responses 族 caller/namespace/async）跨族投给没有对应槽位的
// 客户端协议时被 encodeBlock 静默丢弃。请求侧 describeBlocksLossy 早已报出（lossy.go
// 的 name != ProtocolAnthropic / name != ProtocolResponses 两条门控），响应侧此前静默。
// 本组测非流式描述子 DescribeResponseToolProvenanceLoss 与共用判据 ToolProvenanceDropShape：
// 同族保全不报、跨族才报、标记非空才报（零值不假阳）、两族分账各报一条、多块计数。

const (
	// r64anthrMarker 命中 ResponseToolCallerDropNote（anthropic 族）。
	r64anthrMarker = "caller/toolset_name provenance marker"
	// r64respMarker 命中 ResponseToolRespCallerDropNote（responses 族）。
	r64respMarker = "caller/namespace/async provenance marker"
)

func r64boolP(b bool) *bool { return &b }

// r64anthrTool 造一个带 anthropic 族 caller/toolset_name 的工具调用块。
func r64anthrTool() ir.Block {
	return ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
		ID: "call_1", Name: "grep", Input: `{"q":"x"}`,
		Caller:      []byte(`{"type":"code_execution_20250825"}`),
		ToolsetName: "ts_beta",
	}}
}

// r64respTool 造一个带 responses 族 caller/namespace/async 的工具调用块。
func r64respTool() ir.Block {
	return ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
		ID: "call_2", Name: "shell", Input: `{"cmd":"ls"}`,
		ResponsesCaller:    []byte(`{"type":"program","program":"p1"}`),
		ResponsesNamespace: "ns1",
		ResponsesAsync:     r64boolP(true),
	}}
}

// r64plainTool 造一个不带任何 provenance 标记的普通工具调用块。
func r64plainTool() ir.Block {
	return ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
		ID: "call_3", Name: "f", Input: `{}`}}
}

func r64resp(blocks ...ir.Block) *ir.Response {
	return &ir.Response{ID: "r1", Model: "m", Content: blocks}
}

func r64has(notes []string, sub string) bool {
	for _, s := range notes {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// responses 族标记投给 anthropic 客户端：报 responses 族那条、不报 anthropic 族那条。
func TestR64DescribeRespShapeToAnthropic(t *testing.T) {
	notes := DescribeResponseToolProvenanceLoss(r64resp(r64respTool()), ProtocolAnthropic)
	if !r64has(notes, r64respMarker) {
		t.Errorf("responses 族标记投给 anthropic 客户端没报出：%v", notes)
	}
	if r64has(notes, r64anthrMarker) {
		t.Errorf("anthropic 客户端不该报 anthropic 族（同族保全）：%v", notes)
	}
}

// anthropic 族标记投给 responses 客户端：报 anthropic 族那条、不报 responses 族那条。
func TestR64DescribeAnthrShapeToResponses(t *testing.T) {
	notes := DescribeResponseToolProvenanceLoss(r64resp(r64anthrTool()), ProtocolResponses)
	if !r64has(notes, r64anthrMarker) {
		t.Errorf("anthropic 族标记投给 responses 客户端没报出：%v", notes)
	}
	if r64has(notes, r64respMarker) {
		t.Errorf("responses 客户端不该报 responses 族（同族保全）：%v", notes)
	}
}

// chat 客户端两族槽位都没有：两族标记各报一条（分账）。
func TestR64DescribeBothShapesToChat(t *testing.T) {
	notes := DescribeResponseToolProvenanceLoss(
		r64resp(r64anthrTool(), r64respTool()), ProtocolChatCompletions)
	if !r64has(notes, r64anthrMarker) || !r64has(notes, r64respMarker) {
		t.Errorf("chat 客户端应两族各报一条：%v", notes)
	}
}

// 同族保全：anthropic 族标记投给 anthropic 客户端、responses 族标记投给 responses
// 客户端，都不报。
func TestR64DescribeSameFamilySilent(t *testing.T) {
	if notes := DescribeResponseToolProvenanceLoss(r64resp(r64anthrTool()), ProtocolAnthropic); len(notes) != 0 {
		t.Errorf("anthropic 族标记投给 anthropic 客户端应保全不报：%v", notes)
	}
	if notes := DescribeResponseToolProvenanceLoss(r64resp(r64respTool()), ProtocolResponses); len(notes) != 0 {
		t.Errorf("responses 族标记投给 responses 客户端应保全不报：%v", notes)
	}
}

// 无 provenance 标记的普通工具调用：任何目标都不报（零值不假阳）。
func TestR64DescribePlainToolSilent(t *testing.T) {
	for _, name := range []string{ProtocolAnthropic, ProtocolChatCompletions, ProtocolResponses} {
		if notes := DescribeResponseToolProvenanceLoss(r64resp(r64plainTool()), name); len(notes) != 0 {
			t.Errorf("普通工具调用投给 %s 不该报：%v", name, notes)
		}
	}
}

// nil 响应与无工具块响应：不报、不 panic。
func TestR64DescribeNilAndNoTools(t *testing.T) {
	if notes := DescribeResponseToolProvenanceLoss(nil, ProtocolChatCompletions); notes != nil {
		t.Errorf("nil 响应应返回 nil：%v", notes)
	}
	txt := ir.Block{Type: ir.BlockText, Text: "hi"}
	if notes := DescribeResponseToolProvenanceLoss(r64resp(txt), ProtocolChatCompletions); len(notes) != 0 {
		t.Errorf("无工具块响应不该报：%v", notes)
	}
}

// 多块同族计数汇成一条、数字对。
func TestR64DescribeMultipleCounted(t *testing.T) {
	notes := DescribeResponseToolProvenanceLoss(
		r64resp(r64respTool(), r64respTool(), r64respTool()), ProtocolChatCompletions)
	if !r64has(notes, "3 tool call(s)") {
		t.Errorf("三块 responses 族标记应汇成一条报 3：%v", notes)
	}
}

// 非工具块（server_tool_use / tool_result）不计入：本损类只认 BlockToolUse，
// 与请求侧 describeBlocksLossy 的 tool_use 门控口径一致。
func TestR64DescribeIgnoresNonToolUseBlocks(t *testing.T) {
	srv := ir.Block{Type: ir.BlockServerToolUse, ServerToolUse: &ir.ServerToolUse{
		ID: "s1", Name: "web_search", Caller: []byte(`{"type":"x"}`)}}
	if notes := DescribeResponseToolProvenanceLoss(r64resp(srv), ProtocolChatCompletions); len(notes) != 0 {
		t.Errorf("server_tool_use 不在本损类计数内：%v", notes)
	}
}

// ToolProvenanceDropShape 判据本身：nil 安全、两族独立、同族抑制。
func TestR64ToolProvenanceDropShape(t *testing.T) {
	if a, r := ToolProvenanceDropShape(nil, ProtocolChatCompletions); a || r {
		t.Errorf("nil ToolUse 应两维皆假：%v %v", a, r)
	}
	// responses 族标记 → anthropic 目标：只 responsesShape 为真。
	a, r := ToolProvenanceDropShape(r64respTool().ToolUse, ProtocolAnthropic)
	if a || !r {
		t.Errorf("responses 族标记投 anthropic：want (false,true) got (%v,%v)", a, r)
	}
	// responses 族标记 → responses 目标（同族）：两维皆假。
	a, r = ToolProvenanceDropShape(r64respTool().ToolUse, ProtocolResponses)
	if a || r {
		t.Errorf("responses 族标记投 responses（同族）：want (false,false) got (%v,%v)", a, r)
	}
	// anthropic 族标记 → chat 目标：只 anthropicShape 为真。
	a, r = ToolProvenanceDropShape(r64anthrTool().ToolUse, ProtocolChatCompletions)
	if !a || r {
		t.Errorf("anthropic 族标记投 chat：want (true,false) got (%v,%v)", a, r)
	}
}

// 响应侧措辞与请求侧 toolCallerWhy/respCallerWhy 逐字不等（分账：请求侧讲客户端历史
// 工具调用投给上游被丢，响应侧讲模型输出工具调用投给客户端被丢），但都点明「谁发起」
// 这层归属丢失，且响应侧两条各含其族标志子串以资区分。
func TestR64ResponseWordingDistinctFromRequest(t *testing.T) {
	anthr := ResponseToolCallerDropNote(1)
	resp := ResponseToolRespCallerDropNote(1)
	if !strings.Contains(anthr, "in the model output") || !strings.Contains(resp, "in the model output") {
		t.Errorf("响应侧措辞应点明「模型输出」：\n%q\n%q", anthr, resp)
	}
	if anthr == resp {
		t.Errorf("两族响应侧措辞不应相同")
	}
	if !strings.Contains(anthr, "anthropic-only") || !strings.Contains(resp, "responses-only") {
		t.Errorf("两族措辞应各点明其族：\n%q\n%q", anthr, resp)
	}
}
