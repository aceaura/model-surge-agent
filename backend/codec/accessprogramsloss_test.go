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

// 轮次39：responses 独有的 access_programs（域专属访问计划）跨族损耗。它是
// responses 专属维度、外族无等价槽位：请求侧走 DescribeLossy，responses 自家静默、
// 缺席全静默、外族照实报出。同族原文透传见 responses/accessprograms_test.go。

// apForeign 是三个没有访问计划槽位的出站协议（access_programs 是 responses 独有）。
var apForeign = []string{codec.ProtocolChatCompletions, codec.ProtocolAnthropic, codec.ProtocolGemini}

func apReq() *ir.Request {
	return &ir.Request{Model: "m", MaxTokens: 10,
		Messages:       []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		AccessPrograms: json.RawMessage(`{"cyber":"daybreak_red"}`)}
}

func TestDiagnoseAccessProgramsDroppedOffResponses(t *testing.T) {
	req := apReq()
	for _, name := range apForeign {
		oc, _ := codec.Outbound(name)
		got := strings.Join(codec.DescribeLossy(req, name, oc.Caps()), "; ")
		if !strings.Contains(got, "access_programs") || !strings.Contains(got, "no domain-specific access-program slot") {
			t.Errorf("%s 应报 access_programs 丢失：%q", name, got)
		}
	}
	// responses 自家接得住，静默（就 access_programs 而言——其余维度不在本用例范围）。
	oc, _ := codec.Outbound(codec.ProtocolResponses)
	for _, n := range codec.DescribeLossy(req, codec.ProtocolResponses, oc.Caps()) {
		if strings.Contains(n, "access_programs") {
			t.Errorf("responses 自家误报 access_programs：%v", n)
		}
	}
}

// 缺席时全协议静默。
func TestNoAccessProgramsNoteWhenAbsent(t *testing.T) {
	base := &ir.Request{Model: "m", MaxTokens: 10,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
	for _, name := range codec.OutboundNames() {
		oc, _ := codec.Outbound(name)
		for _, n := range codec.DescribeLossy(base, name, oc.Caps()) {
			if strings.Contains(n, "access_programs") {
				t.Errorf("%s 无 access_programs 却误报：%v", name, n)
			}
		}
	}
}

// 跨族出站编码不得把该键泄漏到外族线格式（丢弃要干净，注记负责告知）；
// 同族 responses 出站必须原样回写（透传兑现）。
func TestAccessProgramsNotWrittenToForeignWire(t *testing.T) {
	req := apReq()
	for _, name := range apForeign {
		out := string(encodeOut(t, name, req))
		if strings.Contains(out, "access_programs") {
			t.Errorf("%s 出站泄漏 access_programs：%s", name, out)
		}
	}
	out := string(encodeOut(t, codec.ProtocolResponses, req))
	if !strings.Contains(out, "access_programs") || !strings.Contains(out, `"cyber":"daybreak_red"`) {
		t.Errorf("responses 同族未回写 access_programs：%s", out)
	}
}
