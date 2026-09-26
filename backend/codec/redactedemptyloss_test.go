package codec_test

import (
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 请求侧有损诊断：涂抹思考块（redacted_thinking）的密文只有 anthropic 同族槽位
// 能逐字承载。同族且密文非空时无损往返，不报；同族但密文为空时编码器整块跳过
// （空 data 会被上游拒收，无从伪造），DescribeLossy 必须先报出来，跳过才不是
// 静默的——与非涂抹空壳思考块（text 与签名皆空）的处置对称。跨族则无论密文是否
// 为空都整块丢弃并报一条。

func redactedRequest(data string) *ir.Request {
	return &ir.Request{
		Model: "m",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockText, Text: "hi"},
			{Type: ir.BlockThinking, Thinking: &ir.Thinking{
				Redacted: true, RedactedData: data, SignatureFrom: codec.ProtocolAnthropic,
			}},
		}}},
	}
}

func TestDescribeLossyReportsSameFamilyEmptyCiphertextRedacted(t *testing.T) {
	oc, _ := codec.Outbound(codec.ProtocolAnthropic)
	notes := codec.DescribeLossy(redactedRequest(""), codec.ProtocolAnthropic, oc.Caps())
	if !hasNoteSubstr(notes, "redacted_thinking") {
		t.Errorf("同族空密文涂抹块被静默跳过，没有有损说明: %v", notes)
	}
}

func TestDescribeLossySilentOnSameFamilyNonEmptyRedacted(t *testing.T) {
	// 同族且密文非空：逐字往返无损，不得报 redacted_thinking。
	oc, _ := codec.Outbound(codec.ProtocolAnthropic)
	notes := codec.DescribeLossy(redactedRequest("opaque"), codec.ProtocolAnthropic, oc.Caps())
	if hasNoteSubstr(notes, "redacted_thinking") {
		t.Errorf("同族非空密文涂抹块本应无损往返，却被误报: %v", notes)
	}
}

func TestDescribeLossyReportsCrossFamilyRedactedRequest(t *testing.T) {
	// 跨族没有密文槽位：无论密文是否非空都整块丢弃并报一条（请求侧）。
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolGemini} {
		oc, ok := codec.Outbound(name)
		if !ok {
			t.Fatalf("Outbound(%s) 不可用", name)
		}
		notes := codec.DescribeLossy(redactedRequest("opaque"), name, oc.Caps())
		if !hasNoteSubstr(notes, "redacted_thinking") {
			t.Errorf("%s 跨族涂抹块没有有损说明: %v", name, notes)
		}
	}
}
