package responses

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// typed tool_choice（mcp/file_search 等无 name 变体）原文进 Raw 不透明槽，
// 同族回写一个字节不动；没给不造键。
func TestTypedToolChoiceRawRoundTrip(t *testing.T) {
	for _, tc := range []string{
		`{"type":"file_search"}`,
		`{"type":"mcp","server_label":"dmcp"}`,
	} {
		body := `{"model":"m","input":"hi","tool_choice":` + tc + `}`
		req, err := DecodeRequest([]byte(body))
		if err != nil {
			t.Fatalf("DecodeRequest(%s): %v", tc, err)
		}
		if req.ToolChoice == nil {
			t.Fatalf("tool_choice 整条被丢：%s", tc)
		}
		if string(req.ToolChoice.Raw) != tc {
			t.Fatalf("Raw = %s, want %s", req.ToolChoice.Raw, tc)
		}
		if req.ToolChoice.Mode != "" {
			t.Fatalf("typed 变体的 Mode = %q, want 零值", req.ToolChoice.Mode)
		}
		out, err := EncodeRequest(req.Clone())
		if err != nil {
			t.Fatalf("EncodeRequest: %v", err)
		}
		if !strings.Contains(string(out), `"tool_choice":`+tc) {
			t.Fatalf("typed tool_choice 没原样回写：%s", out)
		}
	}
	// 没给 tool_choice 不造键。
	plain, err := DecodeRequest([]byte(`{"model":"m","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeRequest(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "tool_choice") {
		t.Fatalf("没给被伪造：%s", out)
	}
}

// 带 type 的已建模指名变体：结构化字段照常供整形与跨族使用，Raw 保原形
// 供同族逐字回写（custom 指名换成 function 会让上游找不到工具）。
func TestNamedTypedToolChoiceKeepsBothSlots(t *testing.T) {
	for _, tc := range []string{
		`{"type":"function","name":"alpha"}`,
		`{"type":"custom","name":"grep"}`,
	} {
		req, err := DecodeRequest([]byte(`{"model":"m","input":"hi","tool_choice":` + tc + `}`))
		if err != nil {
			t.Fatalf("DecodeRequest(%s): %v", tc, err)
		}
		if req.ToolChoice.Mode != ir.ToolChoiceTool {
			t.Fatalf("Mode = %q, want tool（%s）", req.ToolChoice.Mode, tc)
		}
		if string(req.ToolChoice.Raw) != tc {
			t.Fatalf("Raw = %s, want %s", req.ToolChoice.Raw, tc)
		}
		out, err := EncodeRequest(req.Clone())
		if err != nil {
			t.Fatalf("EncodeRequest: %v", err)
		}
		if !strings.Contains(string(out), `"tool_choice":`+tc) {
			t.Fatalf("指名变体没原样回写：%s", out)
		}
	}
	// 无 type 的旧式 {"name":...}：结构化照旧，不造 Raw。
	req, err := DecodeRequest([]byte(`{"model":"m","input":"hi","tool_choice":{"name":"alpha"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.ToolChoice.Raw) != 0 || req.ToolChoice.Name != "alpha" {
		t.Fatalf("旧式指名被改动：Raw=%s Name=%q", req.ToolChoice.Raw, req.ToolChoice.Name)
	}
	out, err := EncodeRequest(req.Clone())
	if err != nil {
		t.Fatal(err)
	}
	// map 序列化按字典序排键，断言跟着排。
	if !strings.Contains(string(out), `"tool_choice":{"name":"alpha","type":"function"}`) {
		t.Fatalf("旧式指名没折进现代槽位：%s", out)
	}
}

// function/custom 缺 name 依然是客户端错误：Raw 收下只会在上游再挨一次
// 400，不如入站就拒。
func TestNamelessModeledChoiceStillRejected(t *testing.T) {
	for _, tc := range []string{`{"type":"function"}`, `{"type":"custom"}`} {
		if _, err := DecodeRequest([]byte(`{"model":"m","input":"hi","tool_choice":` + tc + `}`)); err == nil {
			t.Fatalf("缺 name 的 %s 应被拒", tc)
		}
	}
}
