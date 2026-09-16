package ir

import "testing"

func TestEstimateTokensBounds(t *testing.T) {
	if got := EstimateTokens(""); got != 0 {
		t.Errorf("empty = %d, want 0", got)
	}
	// 短于一个 token 的文本仍占位，否则大量短消息会被整体估成 0。
	if got := EstimateTokens("a"); got != 1 {
		t.Errorf("single char = %d, want at least 1", got)
	}
	if got := EstimateTokens("12345678"); got != 2 {
		t.Errorf("8 chars = %d, want 2", got)
	}
}

// 按 rune 而非 byte 计数：中文一个字符占 3 字节，按字节算会高估 3 倍，
// 那会让策略脚本误判上下文占用而排掉本可用的目标。
func TestEstimateCountsRunesNotBytes(t *testing.T) {
	got := EstimateTokens("中文中文中文中文")
	if got != 2 {
		t.Errorf("8 CJK runes = %d, want 2", got)
	}
}

func TestEstimateRequestCoversAllTextSources(t *testing.T) {
	r := &Request{
		System: []Block{{Type: BlockText, Text: "0123"}},
		Messages: []Message{{
			Role: RoleUser,
			Content: []Block{
				{Type: BlockText, Text: "0123"},
				{Type: BlockThinking, Thinking: &Thinking{Text: "0123"}},
				{Type: BlockToolUse, ToolUse: &ToolUse{Name: "read", Input: "0123"}},
				{Type: BlockToolResult, ToolResult: &ToolResult{
					Content: []Block{{Type: BlockText, Text: "0123"}},
				}},
			},
		}},
		Tools: []Tool{{Name: "read", Description: "0123", Schema: "0123"}},
	}
	// system 1 + text 1 + thinking 1 + tool name 1 + tool input 1 +
	// nested result 1 + tool decl (name 1 + desc 1 + schema 1) = 9
	if got := EstimateRequest(r); got != 9 {
		t.Errorf("estimate = %d, want 9", got)
	}
}

// 图片的 base64 长度与 token 消耗无关，各家按分块计费，
// 按字符估算会得出荒谬的数字。
func TestEstimateIgnoresImagePayloads(t *testing.T) {
	r := &Request{Messages: []Message{{
		Role:    RoleUser,
		Content: []Block{{Type: BlockImage, Image: &Image{Data: string(make([]byte, 4096))}}},
	}}}
	if got := EstimateRequest(r); got != 0 {
		t.Errorf("estimate = %d, image bytes must not inflate the count", got)
	}
}

func TestEstimateNilInputs(t *testing.T) {
	if got := EstimateRequest(nil); got != 0 {
		t.Errorf("nil request = %d", got)
	}
	if got := EstimateResponse(nil); got != 0 {
		t.Errorf("nil response = %d", got)
	}
}

func TestEstimateResponse(t *testing.T) {
	got := EstimateResponse(&Response{Content: []Block{{Type: BlockText, Text: "01234567"}}})
	if got != 2 {
		t.Errorf("estimate = %d, want 2", got)
	}
}
