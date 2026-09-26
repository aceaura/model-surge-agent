package codec_test

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 请求侧有损诊断：目标是 anthropic 时，空壳 thinking 块（无正文且无可写回
// 签名）会被编码器整块跳过，DescribeLossy 必须先报出来，跳过才不是静默的。

func emptyThinkingRequest(t *ir.Thinking) *ir.Request {
	return &ir.Request{
		Model: "m",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockText, Text: "hi"},
			{Type: ir.BlockThinking, Thinking: t},
		}}},
	}
}

func TestDescribeLossyReportsEmptyThinkingShellForAnthropic(t *testing.T) {
	oc, _ := codec.Outbound(codec.ProtocolAnthropic)
	notes := codec.DescribeLossy(emptyThinkingRequest(&ir.Thinking{}),
		codec.ProtocolAnthropic, oc.Caps())
	if !hasNoteSubstr(notes, "empty thinking blocks") {
		t.Errorf("空壳 thinking 块没有有损说明: %v", notes)
	}
}

func TestDescribeLossyReportsForeignSignedEmptyThinkingForAnthropic(t *testing.T) {
	// 异族签名会被剥离，剥完是空壳：同样要报。
	oc, _ := codec.Outbound(codec.ProtocolAnthropic)
	notes := codec.DescribeLossy(emptyThinkingRequest(&ir.Thinking{
		Signature: "enc-xyz", SignatureFrom: codec.ProtocolResponses,
	}), codec.ProtocolAnthropic, oc.Caps())
	if !hasNoteSubstr(notes, "empty thinking blocks") {
		t.Errorf("空正文+异族签名没有空壳说明: %v", notes)
	}
}

func TestDescribeLossySilentOnSignedEmptyThinkingForAnthropic(t *testing.T) {
	// 同族签名可写回：不是空壳，块原样出站，不得报空壳说明。
	oc, _ := codec.Outbound(codec.ProtocolAnthropic)
	notes := codec.DescribeLossy(emptyThinkingRequest(&ir.Thinking{
		Signature: "sig-abc", SignatureFrom: codec.ProtocolAnthropic,
	}), codec.ProtocolAnthropic, oc.Caps())
	if hasNoteSubstr(notes, "empty thinking blocks") {
		t.Errorf("合法的空正文带签名块被误报: %v", notes)
	}
}

func TestDescribeLossyNoEmptyThinkingNoteOffAnthropic(t *testing.T) {
	// 空壳拒收是 Anthropic 专属语义：其余目标的编码器不会因空壳硬失败，
	// 不报这条说明（它们的 thinking 损耗由各自既有条目覆盖）。
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolGemini} {
		oc, ok := codec.Outbound(name)
		if !ok {
			t.Fatalf("Outbound(%s) 不可用", name)
		}
		notes := codec.DescribeLossy(emptyThinkingRequest(&ir.Thinking{}), name, oc.Caps())
		if hasNoteSubstr(notes, "empty thinking blocks") {
			t.Errorf("%s 不该有空壳说明: %v", name, notes)
		}
	}
}

func hasNoteSubstr(notes []string, sub string) bool {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}
