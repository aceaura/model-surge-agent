package gemini

import (
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

func TestConvertFinishReasonCoversFullEnumeration(t *testing.T) {
	cases := map[string]ir.StopReason{
		"STOP":       ir.StopEndTurn,
		"MAX_TOKENS": ir.StopMaxTokens,
		// 内容策略族。
		"SAFETY":             ir.StopContentFilter,
		"RECITATION":         ir.StopContentFilter,
		"BLOCKLIST":          ir.StopContentFilter,
		"PROHIBITED_CONTENT": ir.StopContentFilter,
		"SPII":               ir.StopContentFilter,
		// 「回答没能正常产出」的另外四种说法。
		"OTHER":                     ir.StopContentFilter,
		"MALFORMED_FUNCTION_CALL":   ir.StopContentFilter,
		"LANGUAGE":                  ir.StopContentFilter,
		"IMAGE_SAFETY":              ir.StopContentFilter,
		"FINISH_REASON_UNSPECIFIED": "",
		"":                          "",
		// 未知取值按安全侧兜底。
		"SOME_FUTURE_REASON": ir.StopContentFilter,
	}
	for in, want := range cases {
		if got := convertFinishReason(in); got != want {
			t.Errorf("convertFinishReason(%q) = %q, want %q", in, got, want)
		}
	}
}
