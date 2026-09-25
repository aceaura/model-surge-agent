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

// 思考配置现代化三维（adaptive / display / effort）的跨族与越集诊断。
// adaptive 与 display 是 anthropic 专属，跨族报出；effort 越封闭五值集
// （minimal / 未知值）只在 anthropic 本族报出，"none" 与缺省静默。
//
// 对应旧仓 #28（f2be160）。

func thinkingNotes(t *testing.T, req *ir.Request, name string) string {
	t.Helper()
	oc, ok := codec.Outbound(name)
	if !ok {
		t.Fatalf("outbound %q not registered", name)
	}
	return strings.Join(codec.DescribeLossy(req, name, oc.Caps()), "\n")
}

func TestDiagnoseAdaptiveThinkingDroppedOffAnthropic(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10, Thinking: &ir.ThinkingConfig{
		Enabled: ir.ThinkingOn(), Adaptive: true, Display: "omitted"}}
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolGemini} {
		got := thinkingNotes(t, req, name)
		if !strings.Contains(got, "dropped adaptive thinking") {
			t.Errorf("%s 应报 adaptive 丢失：%q", name, got)
		}
		if !strings.Contains(got, "dropped thinking display preference") {
			t.Errorf("%s 应报 display 丢失：%q", name, got)
		}
	}
	if got := thinkingNotes(t, req, codec.ProtocolAnthropic); got != "" {
		t.Errorf("anthropic 自家接得住，误报：%q", got)
	}
}

func TestDiagnoseAdaptiveSilentWhenAbsent(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10, Thinking: &ir.ThinkingConfig{
		Enabled: ir.ThinkingOn(), BudgetTokens: 4096}}
	for _, name := range codec.OutboundNames() {
		if got := thinkingNotes(t, req, name); got != "" {
			t.Errorf("%s 无 adaptive/display 误报：%q", name, got)
		}
	}
}

func TestDiagnoseEffortValueSetOnAnthropic(t *testing.T) {
	mk := func(effort string) *ir.Request {
		return &ir.Request{Model: "m", MaxTokens: 10, Thinking: &ir.ThinkingConfig{
			Enabled: ir.ThinkingOn(), Effort: effort}}
	}
	got := thinkingNotes(t, mk("minimal"), codec.ProtocolAnthropic)
	if !strings.Contains(got, "dropped minimal thinking effort") {
		t.Errorf("minimal 越集未报：%q", got)
	}
	got = thinkingNotes(t, mk("turbo"), codec.ProtocolAnthropic)
	if !strings.Contains(got, `thinking effort "turbo"`) {
		t.Errorf("未知值未带值报出：%q", got)
	}
	for _, lv := range []string{"", "none", "low", "medium", "high", "xhigh", "max"} {
		if got := thinkingNotes(t, mk(lv), codec.ProtocolAnthropic); got != "" {
			t.Errorf("effort %q 在集内误报：%q", lv, got)
		}
	}
	// 值集诊断只管 anthropic 本族：其余协议 effort 直通或走自己的维度，不报。
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolGemini} {
		if got := thinkingNotes(t, mk("minimal"), name); got != "" {
			t.Errorf("%s 不应做 anthropic 值集诊断：%q", name, got)
		}
	}
}
