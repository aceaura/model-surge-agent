package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 本协议的 tool_use 没有签名字段，所以 gemini 上游给的那一位必然丢在这里。
// 丢是对的，无声地丢不对：运维查一次跨协议工具回合的质量下降时，有损列里
// 必须能看到「上游给过推理凭据，本协议装不下」。
//
// 这条在包内断言而不是只在 codec 包里测那个推导函数：本协议要声明
// toolSigSupported=false，声明成 true 的话推导函数照样正确、这一处照样漏报。
func TestResponseToolSignatureLossIsReported(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{
		Type: ir.BlockToolUse,
		ToolUse: &ir.ToolUse{
			ID: "call_1", Name: "grep", Input: "{}",
			Signature: "sig-from-gemini", SignatureFrom: codec.ProtocolGemini,
		},
	}}}

	_, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	var found string
	for _, n := range notes {
		if strings.Contains(n, "tool call reasoning signature") {
			found = n
		}
	}
	if found == "" {
		t.Fatalf("工具调用签名被无声丢掉了：%#v", notes)
	}
	if !strings.Contains(found, "no signature field on function calls") {
		t.Errorf("理由不对：%q——本协议是「根本没有这个字段」，不是「异族」", found)
	}
}

// 来源恰好是本协议时也要报：本协议没有这个字段，与来源无关。
//
// 单独一条而不是并进上面：声明成 toolSigSupported=true 时上面那条仍会绿
// （来源是异族，照样报一条说明），只有这条会红。
func TestOwnFamilyToolSignatureIsStillReported(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{
		Type: ir.BlockToolUse,
		ToolUse: &ir.ToolUse{
			ID: "call_1", Name: "grep", Input: "{}",
			Signature: "sig-x", SignatureFrom: Name,
		},
	}}}

	_, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	for _, n := range notes {
		if strings.Contains(n, "tool call reasoning signature") {
			return
		}
	}
	t.Fatalf("本协议没有这一字段，来源是谁都得报：%#v", notes)
}
