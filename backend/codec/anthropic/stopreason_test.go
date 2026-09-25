package anthropic

import (
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 全枚举逐条钉住，含未知值分支：未知终止映射成正常结束的话，
// 客户端会照着一段被拦截或截断的内容继续往下走。
func TestConvertStopReasonCoversFullEnumeration(t *testing.T) {
	cases := map[string]ir.StopReason{
		"end_turn":      ir.StopEndTurn,
		"max_tokens":    ir.StopMaxTokens,
		"stop_sequence": ir.StopStopSequence,
		"tool_use":      ir.StopToolUse,
		"refusal":       ir.StopContentFilter,
		// 回合可续跑，语义等同「没说完」。
		"pause_turn": ir.StopMaxTokens,
		// 输入占满窗口挤断输出：独立档位，补救动作与 max_tokens 相反。
		"model_context_window_exceeded": ir.StopContextWindow,
		// 上游没给：留空由聚合层兜底。
		"": "",
		// 未知取值按安全侧兜底。
		"some_future_reason": ir.StopContentFilter,
	}
	for in, want := range cases {
		if got := convertStopReason(in); got != want {
			t.Errorf("convertStopReason(%q) = %q, want %q", in, got, want)
		}
	}
}

// 本协议的取值集覆盖了全部 IR 取值，所以反向映射不该出现空串。
func TestRenderStopReasonCoversEveryIRValue(t *testing.T) {
	for _, s := range []ir.StopReason{
		ir.StopEndTurn, ir.StopMaxTokens, ir.StopStopSequence,
		ir.StopToolUse, ir.StopContentFilter, ir.StopContextWindow,
	} {
		if got := renderStopReason(s); got == "" {
			t.Errorf("renderStopReason(%q) returned empty", s)
		}
	}
}

// context_window 档同族往返原值带回：不并进 max_tokens——两者的客户端
// 补救动作相反（压缩输入 vs 抬输出配额）。
func TestContextWindowStopRoundTrip(t *testing.T) {
	if got := renderStopReason(ir.StopContextWindow); got != "model_context_window_exceeded" {
		t.Fatalf("renderStopReason = %q", got)
	}
	if got := convertStopReason(renderStopReason(ir.StopContextWindow)); got != ir.StopContextWindow {
		t.Fatalf("往返 = %q", got)
	}
}
