package chatcompletions

import (
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

func TestConvertFinishReasonCoversFullEnumeration(t *testing.T) {
	cases := map[string]ir.StopReason{
		"stop":           ir.StopEndTurn,
		"length":         ir.StopMaxTokens,
		"tool_calls":     ir.StopToolUse,
		"function_call":  ir.StopToolUse,
		"content_filter": ir.StopContentFilter,
		// 上游没给：留空由聚合层兜底。
		"": "",
		// 未知取值按安全侧兜底，不当成正常结束。
		"some_future_reason": ir.StopContentFilter,
	}
	for in, want := range cases {
		if got := convertFinishReason(in); got != want {
			t.Errorf("convertFinishReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderFinishReasonCoversEveryIRValue(t *testing.T) {
	cases := map[ir.StopReason]string{
		ir.StopEndTurn:       "stop",
		ir.StopMaxTokens:     "length",
		ir.StopToolUse:       "tool_calls",
		ir.StopContentFilter: "content_filter",
		// 本协议没有单独取值，stop 是最接近的表达。
		ir.StopStopSequence: "stop",
		"":                  "stop",
		// 认不出的 IR 取值不说成正常结束。
		"some_future_reason": "content_filter",
	}
	for in, want := range cases {
		if got := renderFinishReason(in); got != want {
			t.Errorf("renderFinishReason(%q) = %q, want %q", in, got, want)
		}
	}
}
