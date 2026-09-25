package chatcompletions

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// max_completion_tokens 来路键名带回：客户端给的现代键不被换写成官方已
// 废弃的旧键（旧键不兼容 o 系推理模型，换写会直接 400）；旧键客户端的
// wire 原样保留。
func TestMaxCompletionKeyEcho(t *testing.T) {
	modern, err := DecodeRequest([]byte(
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":50}`))
	if err != nil {
		t.Fatal(err)
	}
	if modern.MaxTokens != 50 || !modern.MaxCompletionKey {
		t.Fatalf("MaxTokens/Key = %d/%v, want 50/true", modern.MaxTokens, modern.MaxCompletionKey)
	}
	out, err := EncodeRequest(modern)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"max_completion_tokens":50`) {
		t.Fatalf("现代键没带回：%s", out)
	}
	if strings.Contains(string(out), `"max_tokens":`) {
		t.Fatalf("现代键被换写成旧键：%s", out)
	}

	legacy, err := DecodeRequest([]byte(
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":40}`))
	if err != nil {
		t.Fatal(err)
	}
	if legacy.MaxTokens != 40 || legacy.MaxCompletionKey {
		t.Fatalf("MaxTokens/Key = %d/%v, want 40/false", legacy.MaxTokens, legacy.MaxCompletionKey)
	}
	out2, err := EncodeRequest(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out2), `"max_tokens":40`) {
		t.Fatalf("旧键没保留：%s", out2)
	}
	if strings.Contains(string(out2), "max_completion_tokens") {
		t.Fatalf("旧键被换写成现代键：%s", out2)
	}

	// 非 chat 来源（MaxCompletionKey 恒 false）维持旧键既有形状。
	foreign := &ir.Request{Model: "m", MaxTokens: 30,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
	out3, err := EncodeRequest(foreign)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out3), `"max_tokens":30`) ||
		strings.Contains(string(out3), "max_completion_tokens") {
		t.Fatalf("跨族投影形状被改：%s", out3)
	}
}

// context_window 档投影成 length 而非 stop：输出不完整这一事实必须可见，
// 客户端才不会把半截结果当终稿。
func TestContextWindowRendersLength(t *testing.T) {
	if got := renderFinishReason(ir.StopContextWindow); got != "length" {
		t.Fatalf("renderFinishReason = %q, want length", got)
	}
}
