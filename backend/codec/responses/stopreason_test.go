package responses

import (
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 本协议没有独立的终止原因字段：正常结束看 status，截断看
// incomplete_details.reason，工具调用看 output 里有没有 function_call。
func TestStopReasonForCoversEveryShape(t *testing.T) {
	cases := []struct {
		name string
		resp wireResponse
		want ir.StopReason
	}{
		{"completed", wireResponse{Status: "completed"}, ir.StopEndTurn},
		{"max output tokens",
			wireResponse{Status: "incomplete",
				IncompleteDetails: &wireIncomplete{Reason: "max_output_tokens"}},
			ir.StopMaxTokens},
		{"content filter",
			wireResponse{Status: "incomplete",
				IncompleteDetails: &wireIncomplete{Reason: "content_filter"}},
			ir.StopContentFilter},
		{"unknown incomplete reason falls back to content_filter",
			wireResponse{Status: "incomplete",
				IncompleteDetails: &wireIncomplete{Reason: "some_future_reason"}},
			ir.StopContentFilter},
		{"incomplete without a reason is a truncation",
			wireResponse{Status: "incomplete", IncompleteDetails: &wireIncomplete{}},
			ir.StopMaxTokens},
		{"function_call in output wins over a completed status",
			wireResponse{Status: "completed", Output: []wireRespItem{{Type: itemFunctionCall}}},
			ir.StopToolUse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stopReasonFor(&tc.resp); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
	if got := stopReasonFor(nil); got != "" {
		t.Errorf("nil response should yield no stop reason, got %q", got)
	}
}

// tool_use 必须回 completed：标 incomplete 会让客户端以为回答被截断，
// 于是不去执行 output 里的 function_call，整个工具回合断在这里。
func TestRenderStatusKeepsToolUseDecidable(t *testing.T) {
	cases := map[ir.StopReason]struct {
		status string
		reason string
	}{
		ir.StopEndTurn:   {"completed", ""},
		ir.StopToolUse:   {"completed", ""},
		ir.StopMaxTokens: {"incomplete", "max_output_tokens"},
		// 本协议无对应取值，completed 是最接近的表达。
		ir.StopStopSequence:  {"completed", ""},
		ir.StopContentFilter: {"incomplete", "content_filter"},
		"":                   {"completed", ""},
		// 认不出的 IR 取值不说成正常结束。
		"some_future_reason": {"incomplete", "content_filter"},
	}
	for in, want := range cases {
		status, incomplete := renderStatus(in)
		if status != want.status {
			t.Errorf("renderStatus(%q) status = %q, want %q", in, status, want.status)
		}
		got := ""
		if incomplete != nil {
			got = incomplete.Reason
		}
		if got != want.reason {
			t.Errorf("renderStatus(%q) reason = %q, want %q", in, got, want.reason)
		}
	}
}
