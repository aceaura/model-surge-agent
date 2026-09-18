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
		ir.StopToolUse, ir.StopContentFilter,
	} {
		if got := renderStopReason(s); got == "" {
			t.Errorf("renderStopReason(%q) returned empty", s)
		}
	}
}
