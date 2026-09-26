package gemini

import (
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// TestToolUsePromptTokensFoldIntoOutput 工具调用消耗（toolUsePromptTokenCount）
// 是 totalTokenCount 的独立加项、不含在 candidatesTokenCount 里，计费上属于输出。
// 此前全仓无人接收，工具回合的输出被系统性少计。这里钉住它并入 OutputTokens，
// 且不动 InputTokens（它不是输入侧的量，尽管名字里带 Prompt）。
func TestToolUsePromptTokensFoldIntoOutput(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"ok"}]}}],` +
		`"usageMetadata":{"promptTokenCount":100,"cachedContentTokenCount":20,` +
		`"candidatesTokenCount":30,"thoughtsTokenCount":5,"toolUsePromptTokenCount":7,` +
		`"totalTokenCount":142}}`)
	resp, _, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	u := resp.Usage
	// 输出 = candidates 30 + thoughts 5 + toolUse 7 = 42
	if u.OutputTokens != 42 {
		t.Errorf("OutputTokens 应含工具调用消耗，want 42 got %d", u.OutputTokens)
	}
	// 输入 = prompt 100 − cached 20 = 80，工具消耗不得混进输入。
	if u.InputTokens != 80 {
		t.Errorf("InputTokens 不应受工具消耗影响，want 80 got %d", u.InputTokens)
	}
	if u.ReasoningTokens != 5 {
		t.Errorf("ReasoningTokens 仍只记 thoughts，want 5 got %d", u.ReasoningTokens)
	}
}

// TestToolUsePromptTokensInStream 流式路径与非流式同口径。
func TestToolUsePromptTokensInStream(t *testing.T) {
	dec := newStreamDecoder()
	if _, err := dec.Feed("", `{"candidates":[{"content":{"parts":[{"text":"hi"}]}}],`+
		`"usageMetadata":{"candidatesTokenCount":10,"toolUsePromptTokenCount":4}}`); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	var u *ir.Usage
	for _, ev := range dec.Finish() {
		if ev.Type == ir.EvMessageDelta && ev.Usage != nil {
			u = ev.Usage
		}
	}
	if u == nil {
		t.Fatalf("流式应带出 usage")
	}
	if u.OutputTokens != 14 {
		t.Errorf("流式 OutputTokens 应含工具消耗，want 14 got %d", u.OutputTokens)
	}
}
