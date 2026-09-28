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
		// 服务端工具暂停、回合可续跑：独立档位，补救动作（原样续提回合）与
		// max_tokens（抬输出配额）相反。
		"pause_turn": ir.StopPauseTurn,
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
		ir.StopMaxMessages, ir.StopPauseTurn,
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

// pause_turn 档同族往返原值带回：不并进 max_tokens——pause_turn 要把半截回合
// 原样续提让服务端工具接着跑，max_tokens 要抬输出配额，补救动作相反。此前折成
// StopMaxTokens 会让 anthropic→anthropic 直通的 agent 客户端去加预算而非续跑。
func TestPauseTurnStopRoundTrip(t *testing.T) {
	if got := renderStopReason(ir.StopPauseTurn); got != "pause_turn" {
		t.Fatalf("renderStopReason = %q", got)
	}
	if got := convertStopReason(renderStopReason(ir.StopPauseTurn)); got != ir.StopPauseTurn {
		t.Fatalf("往返 = %q", got)
	}
}
