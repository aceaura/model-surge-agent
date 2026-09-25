package codec

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

func badArgsBlock() ir.Block {
	return ir.Block{Type: ir.BlockToolUse,
		ToolUse: &ir.ToolUse{ID: "c1", Name: "grep", Input: `{"pattern":"x`}}
}

// 请求侧：畸形工具参数与协议无关，任何出站都报告；措辞按槽位形态分开
// ——对象槽位挪键（工具拿不到参数），字符串槽位原文透出（工具解析失败）。
func TestDescribeLossyMalformedToolArgs(t *testing.T) {
	req := ir.Request{Messages: msg(badArgsBlock())}

	got := DescribeLossy(&req, "self", fullCaps())
	if !hasNote(got, "passed through 1 malformed tool call argument(s) verbatim") {
		t.Errorf("字符串槽位未报透传：%v", got)
	}

	objCaps := fullCaps()
	objCaps.ToolInputObject = true
	got = DescribeLossy(&req, "self", objCaps)
	if !hasNote(got, "rewrapped 1 tool call argument(s)") ||
		!hasNote(got, ir.RawArgsKey) {
		t.Errorf("对象槽位未报挪键：%v", got)
	}

	// 合法对象入参不触发任何一条。
	okReq := ir.Request{Messages: msg(ir.Block{Type: ir.BlockToolUse,
		ToolUse: &ir.ToolUse{ID: "c1", Name: "grep", Input: `{"pattern":"x"}`}})}
	for _, caps := range []Capabilities{fullCaps(), objCaps} {
		if got := DescribeLossy(&okReq, "self", caps); hasNote(got, "tool call argument") {
			t.Errorf("合法入参不该报：%v", got)
		}
	}
}

// 各出站协议的能力位声明要与它的 wire 形态一致：anthropic 的 tool_use.input
// 与 gemini 的 functionCall.args 是对象槽位，chat 的 arguments 与 responses
// 的 arguments 是字符串槽位。声明错了诊断措辞就指错方向。
func TestOutboundCapsToolInputObjectMatchesWire(t *testing.T) {
	want := map[string]bool{
		ProtocolAnthropic:       true,
		ProtocolGemini:          true,
		ProtocolChatCompletions: false,
		ProtocolResponses:       false,
	}
	for name, obj := range want {
		c, ok := Outbound(name)
		if !ok {
			t.Fatalf("outbound %q 未注册", name)
		}
		if got := c.Caps().ToolInputObject; got != obj {
			t.Errorf("%s: ToolInputObject = %v, want %v", name, got, obj)
		}
	}
}

// 响应侧共享推导：对象槽位报挪键（RewrapNote），字符串槽位报原文透传
// （RawArgsPassNote），合法入参两侧都沉默。
func TestDescribeResponseToolArgsLoss(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{badArgsBlock()}}

	obj := DescribeResponseToolArgsLoss(resp, true)
	if len(obj) != 1 || !strings.Contains(obj[0], "rewrapped 1 malformed") ||
		!strings.Contains(obj[0], ir.RawArgsKey) {
		t.Errorf("对象槽位说明不对：%v", obj)
	}
	str := DescribeResponseToolArgsLoss(resp, false)
	if len(str) != 1 || !strings.Contains(str[0], "preserved 1 malformed") {
		t.Errorf("字符串槽位说明不对：%v", str)
	}

	okResp := &ir.Response{Content: []ir.Block{{Type: ir.BlockToolUse,
		ToolUse: &ir.ToolUse{ID: "c1", Name: "grep", Input: `{"a":1}`}}}}
	for _, objectSlot := range []bool{true, false} {
		if got := DescribeResponseToolArgsLoss(okResp, objectSlot); got != nil {
			t.Errorf("合法入参 objectSlot=%v 不该报：%v", objectSlot, got)
		}
	}
	// 空输入是无参调用，不是畸形。
	empty := &ir.Response{Content: []ir.Block{{Type: ir.BlockToolUse,
		ToolUse: &ir.ToolUse{ID: "c1", Name: "grep"}}}}
	if got := DescribeResponseToolArgsLoss(empty, true); got != nil {
		t.Errorf("空入参不该报：%v", got)
	}
}

func hasNote(notes []string, sub string) bool {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}
