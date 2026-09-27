package codec

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次14 两个真缺口的全覆盖测试。两处都是跨族时确有丢弃/改写却无人报（rule a）：
//
//	A-3 端用户标识 Metadata["user_id"]：anthropic 映进 metadata.user_id、
//	    chat/responses 落顶层 user 字段，唯独 gemini 没有任何用户标识槽位，整条丢弃。
//	A-6 思考强度两种表达互折：budget→effort（chat/responses 把精确 token 预算折成
//	    粗档位，rewrote）与 effort→budget（anthropic 非 adaptive / gemini 把档位兜底
//	    成 token 预算，filled），两个方向此前都无注记。

func userIDReq() *ir.Request {
	return &ir.Request{Model: "m", MaxTokens: 16, Metadata: map[string]string{"user_id": "u1"}}
}

func thinkingReq(mut func(*ir.ThinkingConfig)) *ir.Request {
	t := &ir.ThinkingConfig{Enabled: ir.ThinkingOn()}
	mut(t)
	return &ir.Request{Model: "m", MaxTokens: 16, Thinking: t}
}

// ---- A-3: 端用户标识 ----

const userIDNote = "no end-user identifier slot"

func TestDescribeLossyUserIDNotedForGemini(t *testing.T) {
	got := strings.Join(DescribeLossy(userIDReq(), ProtocolGemini, fullCaps()), "\n")
	if !strings.Contains(got, userIDNote) {
		t.Errorf("gemini 没有用户标识槽位，丢弃 user_id 应报注记:\n%s", got)
	}
}

// anthropic/chat/responses 都能投递 user_id，不得误报。
func TestDescribeLossyUserIDSilentForDeliveringProtocols(t *testing.T) {
	for _, name := range []string{ProtocolAnthropic, ProtocolChatCompletions, ProtocolResponses} {
		got := strings.Join(DescribeLossy(userIDReq(), name, fullCaps()), "\n")
		if strings.Contains(got, userIDNote) {
			t.Errorf("%s 能投递 user_id，被误报丢弃:\n%s", name, got)
		}
	}
}

// 没有 user_id 时即便目标是 gemini 也不得报。
func TestDescribeLossyUserIDSilentWhenAbsent(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 16}
	got := strings.Join(DescribeLossy(req, ProtocolGemini, fullCaps()), "\n")
	if strings.Contains(got, userIDNote) {
		t.Errorf("无 user_id 的请求被误报丢弃端用户标识:\n%s", got)
	}
}

// ---- A-6: 思考预算↔档位折叠 ----

const (
	budgetRewroteNote = "folded into an effort tier"
	budgetFilledNote  = "synthesized because the request carried none"
)

// budget→effort：客户端给了精确预算、没给档位，目标是 chat/responses 时折成档位。
func TestDescribeLossyBudgetFoldedToEffortNoted(t *testing.T) {
	req := thinkingReq(func(t *ir.ThinkingConfig) { t.BudgetTokens = 8000 })
	for _, name := range []string{ProtocolChatCompletions, ProtocolResponses} {
		got := strings.Join(DescribeLossy(req, name, fullCaps()), "\n")
		if !strings.Contains(got, budgetRewroteNote) {
			t.Errorf("%s 把精确思考预算折成粗档位，应报 rewrote 注记:\n%s", name, got)
		}
	}
}

// 同请求发到 anthropic/gemini（保留预算，不折档位）不得报 budget→effort。
func TestDescribeLossyBudgetFoldSilentForBudgetProtocols(t *testing.T) {
	req := thinkingReq(func(t *ir.ThinkingConfig) { t.BudgetTokens = 8000 })
	for _, name := range []string{ProtocolAnthropic, ProtocolGemini} {
		got := strings.Join(DescribeLossy(req, name, fullCaps()), "\n")
		if strings.Contains(got, budgetRewroteNote) {
			t.Errorf("%s 原生用 token 预算、不折档位，被误报 budget→effort:\n%s", name, got)
		}
	}
}

// effort→budget：客户端给了档位、没给预算，目标是 anthropic(非 adaptive)/gemini 时兜底预算。
func TestDescribeLossyEffortFilledToBudgetNoted(t *testing.T) {
	req := thinkingReq(func(t *ir.ThinkingConfig) { t.Effort = "high" })
	for _, name := range []string{ProtocolAnthropic, ProtocolGemini} {
		got := strings.Join(DescribeLossy(req, name, fullCaps()), "\n")
		if !strings.Contains(got, budgetFilledNote) {
			t.Errorf("%s 用 token 预算表达思考强度、缺预算须兜底，应报 filled 注记:\n%s", name, got)
		}
	}
}

// 同请求发到 chat/responses（档位直通，不需预算）不得报 effort→budget。
func TestDescribeLossyEffortFillSilentForEffortProtocols(t *testing.T) {
	req := thinkingReq(func(t *ir.ThinkingConfig) { t.Effort = "high" })
	for _, name := range []string{ProtocolChatCompletions, ProtocolResponses} {
		got := strings.Join(DescribeLossy(req, name, fullCaps()), "\n")
		if strings.Contains(got, budgetFilledNote) {
			t.Errorf("%s 档位直通、不兜底预算，被误报 effort→budget:\n%s", name, got)
		}
	}
}

// anthropic 的 adaptive 档不带预算（模型自主决定），编码器不兜底，故不得报 filled；
// gemini 没有 adaptive 概念、恒兜底预算，故仍报。
func TestDescribeLossyAdaptiveBudgetFillScope(t *testing.T) {
	req := thinkingReq(func(t *ir.ThinkingConfig) { t.Adaptive = true })

	anth := strings.Join(DescribeLossy(req, ProtocolAnthropic, fullCaps()), "\n")
	if strings.Contains(anth, budgetFilledNote) {
		t.Errorf("anthropic adaptive 档不带预算、不兜底，被误报 filled:\n%s", anth)
	}
	gem := strings.Join(DescribeLossy(req, ProtocolGemini, fullCaps()), "\n")
	if !strings.Contains(gem, budgetFilledNote) {
		t.Errorf("gemini 无 adaptive 概念、恒兜底预算，应报 filled:\n%s", gem)
	}
}

// 预算与档位都给全时：两个方向都不发生（chat 直接用档位、anthropic 直接用预算）。
func TestDescribeLossyBothBudgetAndEffortSilent(t *testing.T) {
	req := thinkingReq(func(t *ir.ThinkingConfig) { t.BudgetTokens = 8000; t.Effort = "high" })
	for _, name := range []string{ProtocolAnthropic, ProtocolChatCompletions, ProtocolResponses, ProtocolGemini} {
		got := strings.Join(DescribeLossy(req, name, fullCaps()), "\n")
		if strings.Contains(got, budgetRewroteNote) || strings.Contains(got, budgetFilledNote) {
			t.Errorf("%s 预算与档位俱全、无需折叠或兜底，被误报:\n%s", name, got)
		}
	}
}
