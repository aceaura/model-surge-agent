package anthropic

import (
	"strings"
	"testing"
)

// 未知 tool_choice.type 必须 400，而不是静默回落 nil（那会把「强制某工具」
// 悄悄降级成 auto）。与 chat_completions / responses 两族同口径。
func TestUnknownToolChoiceTypeRejected(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"tool_choice":{"type":"bogus"}}`)
	_, err := DecodeRequest(body)
	if err == nil {
		t.Fatalf("未知 tool_choice.type 应报错，却通过了")
	}
	if !strings.Contains(err.Error(), "tool_choice") {
		t.Errorf("错误应指明 tool_choice 字段，实得 %v", err)
	}
}

// 已知的四种 type 仍照常解码，空 tool_choice 不报错。
func TestKnownToolChoiceTypesAccepted(t *testing.T) {
	for _, tc := range []string{`"auto"`, `"any"`, `"none"`} {
		body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":` + tc + `}}`)
		if _, err := DecodeRequest(body); err != nil {
			t.Errorf("tool_choice.type=%s 不该报错: %v", tc, err)
		}
	}
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"tool","name":"grep"}}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatalf("tool 型 tool_choice 不该报错: %v", err)
	}
	if req.ToolChoice == nil || req.ToolChoice.Name != "grep" {
		t.Errorf("tool 型 tool_choice 未解出名字: %+v", req.ToolChoice)
	}
}
