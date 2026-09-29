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
		got := thinkingNotes(t, req, name)
		// 本用例只验 adaptive/display 两维在 adaptive 缺席时静默。预算折档
		// （轮次14 A-6：chat/responses 把 4096 折成粗档位）是另一维的合法注记，
		// 不在本断言范围，故只查 adaptive/display 两个子串而非整段为空。
		if strings.Contains(got, "adaptive thinking") ||
			strings.Contains(got, "thinking display preference") {
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
		got := thinkingNotes(t, mk(lv), codec.ProtocolAnthropic)
		// 只验 effort 值集诊断：集内值不报越集。这些请求带了档位却没带预算，
		// anthropic 须合成预算（轮次14 A-6 的 filled 注记，与 max_tokens 兜底
		// 同款），那是另一维的合法注记，故查 "thinking effort" 子串而非整段为空。
		if strings.Contains(got, "thinking effort") {
			t.Errorf("effort %q 在集内误报值集诊断：%q", lv, got)
		}
	}
	// 值集诊断只管 anthropic 本族：其余协议 effort 直通或走自己的维度，不报
	// 值集诊断（gemini 缺预算会合成预算，那是 A-6 的 filled 注记，非值集诊断）。
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolGemini} {
		if got := thinkingNotes(t, mk("minimal"), name); strings.Contains(got, "thinking effort") {
			t.Errorf("%s 不应做 anthropic 值集诊断：%q", name, got)
		}
	}
}

// 轮次80：chat/responses 请求编码「开启思考但既无档位也无预算」时，编码器用
// effortForBudget(0) 合成一个 medium 档送到上游——这是替客户端发明了一个它从没
// 说过的强度值，此前无任何注记（谓词1 要求 BudgetTokens>0，谓词2 排除 chat/
// responses），而 gemini/anthropic-非adaptive 对同类「合成强度」早有 filled 注记。
// 违反规则 a（合成/兜底改了客户端没给的东西必须留痕，对齐 max_tokens filled 纪律）
// 与规则 c（跨族同类丢失应同种处置——都用 filled）。触发路径真实可达：Anthropic
// 入站 thinking:{"type":"enabled"} 不带 budget_tokens（decode_request 不报 400，
// BudgetTokens=0、Effort=""）路由到 chat_completions / responses 上游。
func TestZeroBudgetEnabledThinkingSynthesizesEffortNote(t *testing.T) {
	// Effort="" 且 BudgetTokens=0，明确开启——正是 effortForBudget(0)→medium 的输入。
	req := &ir.Request{Model: "m", MaxTokens: 10, Thinking: &ir.ThinkingConfig{
		Enabled: ir.ThinkingOn()}}
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses} {
		got := thinkingNotes(t, req, name)
		// 必须是 filled 处置（合成），不是 rewrote（没有预算可折），也不是 dropped。
		if !strings.Contains(got, "filled in thinking effort") {
			t.Errorf("%s 零预算开启思考应报合成档位的 filled 注记：%q", name, got)
		}
		// 不能误报成「预算被折叠」——客户端根本没给预算。
		if strings.Contains(got, "thinking budget") {
			t.Errorf("%s 零预算不该报 thinking budget（无预算可折）：%q", name, got)
		}
	}
}

func TestZeroBudgetEffortNoteIsCrossFamilyConsistent(t *testing.T) {
	// 同一「客户端开了思考却没给强度、目标需合成一个」的丢失类：chat/responses
	// 合成档位、gemini 合成预算，两者都必须是 filled 处置（规则 c）。
	req := &ir.Request{Model: "m", MaxTokens: 10, Thinking: &ir.ThinkingConfig{
		Enabled: ir.ThinkingOn()}}
	chat := thinkingNotes(t, req, codec.ProtocolChatCompletions)
	gemini := thinkingNotes(t, req, codec.ProtocolGemini)
	if !strings.Contains(chat, "filled in thinking effort") {
		t.Errorf("chat 侧应 filled 合成档位：%q", chat)
	}
	if !strings.Contains(gemini, "filled in thinking budget") {
		t.Errorf("gemini 侧应 filled 合成预算：%q", gemini)
	}
}

func TestBudgetGivenStillRewritesNotFills(t *testing.T) {
	// 回归钉住：给了精确预算时仍是 rewrote（折叠），不是 filled（合成）。
	req := &ir.Request{Model: "m", MaxTokens: 10, Thinking: &ir.ThinkingConfig{
		Enabled: ir.ThinkingOn(), BudgetTokens: 4096}}
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses} {
		got := thinkingNotes(t, req, name)
		if !strings.Contains(got, "rewrote thinking budget") {
			t.Errorf("%s 有预算应报 rewrote 折叠：%q", name, got)
		}
		if strings.Contains(got, "filled in thinking effort") {
			t.Errorf("%s 有预算不该报零预算合成注记：%q", name, got)
		}
	}
}

func TestEffortGivenNeedsNoSynthesisNote(t *testing.T) {
	// 回归钉住：客户端给了档位（Effort 直通），编码器不合成，故无 filled/rewrote。
	req := &ir.Request{Model: "m", MaxTokens: 10, Thinking: &ir.ThinkingConfig{
		Enabled: ir.ThinkingOn(), Effort: "low"}}
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses} {
		got := thinkingNotes(t, req, name)
		if strings.Contains(got, "filled in thinking effort") ||
			strings.Contains(got, "thinking budget") {
			t.Errorf("%s 给了档位不该报合成/折叠：%q", name, got)
		}
	}
}

func TestAdaptiveZeroBudgetAnthropicNoSynthesisNote(t *testing.T) {
	// 回归钉住：anthropic adaptive 由模型自主决定思考量、不带预算，是 native，
	// 不合成预算也不报注记（谓词2 用 !Adaptive 排除）。
	req := &ir.Request{Model: "m", MaxTokens: 10, Thinking: &ir.ThinkingConfig{
		Enabled: ir.ThinkingOn(), Adaptive: true}}
	if got := thinkingNotes(t, req, codec.ProtocolAnthropic); got != "" {
		t.Errorf("anthropic adaptive 自家接得住，误报：%q", got)
	}
}
