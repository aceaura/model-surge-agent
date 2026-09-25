package codec_test

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	_ "github.com/aceaura/model-surge-agent/backend/codec/anthropic"
	_ "github.com/aceaura/model-surge-agent/backend/codec/chatcompletions"
	_ "github.com/aceaura/model-surge-agent/backend/codec/gemini"
	_ "github.com/aceaura/model-surge-agent/backend/codec/responses"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 历史消息里的来源标注能不能活下来，取决于目标协议有没有槽位。
// anthropic（text.citations）、chat_completions（message.annotations）、
// responses（output_text.annotations）都有；gemini 的 groundingMetadata
// 只在它的客户端方向存在，本服务对 gemini 只有出站请求侧，没有落点，
// 必须报出丢弃而不是静默抹掉。
func citationRequest(n int) *ir.Request {
	cs := make([]ir.Citation, 0, n)
	for i := 0; i < n; i++ {
		cs = append(cs, ir.Citation{URL: "https://src.test/" + string(rune('a'+i)), Start: 0, End: 1})
	}
	return &ir.Request{
		Model: "m",
		Messages: []ir.Message{{
			Role:    ir.RoleAssistant,
			Content: []ir.Block{{Type: ir.BlockText, Text: "结论", Citations: cs}},
		}},
	}
}

func TestDiagnoseCitationPerProtocol(t *testing.T) {
	req := citationRequest(1)
	for _, name := range codec.OutboundNames() {
		oc, ok := codec.Outbound(name)
		if !ok {
			t.Fatalf("outbound %q not registered", name)
		}
		notes := codec.DescribeLossy(req, name, oc.Caps())
		joined := strings.Join(notes, "\n")
		hasNote := strings.Contains(joined, "citation(s)")
		switch name {
		case codec.ProtocolGemini:
			if !hasNote {
				t.Errorf("gemini must report dropped citations, notes = %v", notes)
			}
			if !strings.Contains(joined, "dropped 1 citation(s)") {
				t.Errorf("gemini note wrong: %s", joined)
			}
		default:
			if hasNote {
				t.Errorf("%s has a citation slot, must stay silent: %s", name, joined)
			}
		}
	}
}

// 计数要覆盖全部消息的全部块，不是每消息一条。
func TestDiagnoseCitationCountsAll(t *testing.T) {
	req := &ir.Request{Model: "m", Messages: []ir.Message{
		{Role: ir.RoleAssistant, Content: []ir.Block{
			{Type: ir.BlockText, Text: "a", Citations: []ir.Citation{{URL: "https://a"}}},
			{Type: ir.BlockText, Text: "b", Citations: []ir.Citation{{URL: "https://b"}}},
		}},
		{Role: ir.RoleAssistant, Content: []ir.Block{
			{Type: ir.BlockText, Text: "c", Citations: []ir.Citation{{URL: "https://c"}}},
		}},
	}}
	oc, _ := codec.Outbound(codec.ProtocolGemini)
	joined := strings.Join(codec.DescribeLossy(req, codec.ProtocolGemini, oc.Caps()), "\n")
	if !strings.Contains(joined, "dropped 3 citation(s)") {
		t.Errorf("want a 3-citation note, got: %s", joined)
	}
}

// 没有引用时不出说明：恒定出现的注记会淹没真正丢了东西的那几条。
func TestDiagnoseNoCitationNoNote(t *testing.T) {
	req := &ir.Request{Model: "m", Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
	}}
	for _, name := range codec.OutboundNames() {
		oc, _ := codec.Outbound(name)
		for _, note := range codec.DescribeLossy(req, name, oc.Caps()) {
			if strings.Contains(note, "citation(s)") {
				t.Errorf("%s reported citations that do not exist: %s", name, note)
			}
		}
	}
}
