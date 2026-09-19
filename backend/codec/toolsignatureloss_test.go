package codec

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 本文件守「工具调用签名在回客户端时的剥离说明」。判据 14–15。
//
// 三个入站协议的 tool_use 都没有签名字段，所以 gemini 上游给的那一位必然
// 丢在这里。丢是对的，无声地丢不对：运维查一次跨协议工具回合的质量下降时，
// 有损列里必须能看到「上游给过推理凭据，本协议装不下」。

func toolResp(sig, from string) *ir.Response {
	return &ir.Response{Content: []ir.Block{{
		Type: ir.BlockToolUse,
		ToolUse: &ir.ToolUse{
			ID: "call_1", Name: "grep", Input: "{}",
			Signature: sig, SignatureFrom: from,
		},
	}}}
}

// 判据 14：没有签名字段的协议一律剥离并说明，与来源无关。
func TestToolSignatureLossIsReportedWhenProtocolHasNoField(t *testing.T) {
	for _, from := range []string{ProtocolGemini, ProtocolAnthropic, ""} {
		notes := DescribeResponseToolSignatureLoss(toolResp("sig-x", from), ProtocolAnthropic, false)
		if len(notes) != 1 {
			t.Fatalf("from=%q notes = %#v，want 一条说明", from, notes)
		}
		if !strings.Contains(notes[0], "tool call reasoning signature") {
			t.Errorf("说明没点明丢的是工具调用那一处：%q", notes[0])
		}
		// 措辞必须与思考块那一条可区分：一条响应里两处都可能丢，
		// 措辞相同的话运维看不出丢的是哪个。
		if strings.Contains(notes[0], "thinking signature") {
			t.Errorf("与思考块的说明混同了：%q", notes[0])
		}
	}
}

// 判据 15：本协议支持这一位且来源同族时不报，异族仍报。
func TestToolSignatureLossRespectsFamilyWhenSupported(t *testing.T) {
	if notes := DescribeResponseToolSignatureLoss(
		toolResp("sig-x", ProtocolGemini), ProtocolGemini, true); len(notes) != 0 {
		t.Errorf("同族透传不算丢弃：%#v", notes)
	}
	if notes := DescribeResponseToolSignatureLoss(
		toolResp("sig-x", ProtocolAnthropic), ProtocolGemini, true); len(notes) != 1 {
		t.Errorf("异族密文必须剥离并说明，notes = %#v", notes)
	}
}

// 上游没给签名时一条说明都不出：每次普通工具调用都报会把真正的丢弃淹掉。
func TestNoToolSignatureMeansNoNote(t *testing.T) {
	if notes := DescribeResponseToolSignatureLoss(
		toolResp("", ""), ProtocolAnthropic, false); len(notes) != 0 {
		t.Errorf("上游没给不是丢弃：%#v", notes)
	}
	// ToolUse 为 nil 的畸形块不该 panic，也不该报。
	resp := &ir.Response{Content: []ir.Block{{Type: ir.BlockToolUse}}}
	if notes := DescribeResponseToolSignatureLoss(resp, ProtocolAnthropic, false); len(notes) != 0 {
		t.Errorf("空块产出了说明：%#v", notes)
	}
}
