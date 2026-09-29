package chatcompletions

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// allowed_tools 是本协议合法的白名单 tool_choice 形态（官方
// ChatCompletionAllowedToolChoiceParam：嵌套 {type,allowed_tools:{mode,tools}}）。
// 此前对象分支只认 function.name，这个形态没有，于是整个 tool_choice 被
// "function.name is required" 400 拒掉——客户端既拿不到限制也拿不到注记。
// 现在必须解成 Mode + AllowedTools（出站靠 ShapeRequest 收窄工具列表等价实现）。
func TestAllowedToolsToolChoiceDecodes(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want ir.ToolChoiceMode
	}{
		{"required", ir.ToolChoiceAny},
		{"auto", ir.ToolChoiceAuto},
	} {
		body := `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
			`"tools":[{"type":"function","function":{"name":"alpha"}},{"type":"function","function":{"name":"beta"}}],` +
			`"tool_choice":{"type":"allowed_tools","allowed_tools":{"mode":"` + tc.mode + `",` +
			`"tools":[{"type":"function","function":{"name":"alpha"}},{"type":"function","function":{"name":"beta"}}]}}}`
		req, err := DecodeRequest([]byte(body))
		if err != nil {
			t.Fatalf("allowed_tools(mode=%s) 被拒：%v", tc.mode, err)
		}
		if req.ToolChoice == nil {
			t.Fatalf("allowed_tools(mode=%s) 整条被丢", tc.mode)
		}
		if req.ToolChoice.Mode != tc.want {
			t.Fatalf("mode=%s → Mode=%q, want %q", tc.mode, req.ToolChoice.Mode, tc.want)
		}
		if got := req.ToolChoice.AllowedTools; len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
			t.Fatalf("mode=%s → AllowedTools=%v, want [alpha beta]", tc.mode, got)
		}
		// 白名单不进 Raw：它是结构化维度，靠收窄工具列表实现，不是不透明原文。
		if len(req.ToolChoice.Raw) != 0 || req.ToolChoice.RawFamily != "" {
			t.Fatalf("allowed_tools 误进 Raw 槽：Raw=%s RawFamily=%q", req.ToolChoice.Raw, req.ToolChoice.RawFamily)
		}
	}
}

// custom 指名自定义工具（官方 ChatCompletionNamedToolChoiceCustomParam：嵌套
// {type,custom:{name}}）。此前同样被 "function.name is required" 400 拒掉。
// 现在解成 Mode/Name 结构化字段供整形与跨族使用，原文进 Raw 并标 RawFamily
// =本族，只有 chat 出站逐字回写（custom 指名换成 function 会让上游在函数表
// 里找不到这个自定义工具）。
func TestCustomToolChoiceKeepsBothSlots(t *testing.T) {
	const raw = `{"type":"custom","custom":{"name":"grep"}}`
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"custom","custom":{"name":"grep"}}],"tool_choice":` + raw + `}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("custom 被拒：%v", err)
	}
	if req.ToolChoice.Mode != ir.ToolChoiceTool || req.ToolChoice.Name != "grep" {
		t.Fatalf("custom 结构化字段错：Mode=%q Name=%q", req.ToolChoice.Mode, req.ToolChoice.Name)
	}
	if string(req.ToolChoice.Raw) != raw {
		t.Fatalf("custom Raw = %s, want %s", req.ToolChoice.Raw, raw)
	}
	if req.ToolChoice.RawFamily != Name {
		t.Fatalf("custom RawFamily = %q, want %q", req.ToolChoice.RawFamily, Name)
	}
	// 同族出站逐字回写：嵌套 custom 形状一个字节不动。
	out, err := EncodeRequest(req.Clone())
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(out), `"tool_choice":`+raw) {
		t.Fatalf("custom 同族没原样回写：%s", out)
	}
}

// custom 缺 name 依然是客户端错误：Raw 收下只会在上游再挨一次 400，不如入站
// 就拒。与 responses 的 TestNamelessModeledChoiceStillRejected 同口径。
func TestNamelessCustomStillRejected(t *testing.T) {
	for _, tc := range []string{
		`{"type":"custom"}`,
		`{"type":"custom","custom":{}}`,
		`{"type":"custom","custom":{"name":""}}`,
	} {
		body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"tool_choice":` + tc + `}`
		if _, err := DecodeRequest([]byte(body)); err == nil {
			t.Fatalf("缺 name 的 %s 应被拒", tc)
		}
	}
}

// type 分派不得动到既有的 function / 扁平废弃形态：现代 function 指名照旧解成
// Mode/Name（无 Raw，编码器重建同形），废弃扁平 {"name":"x"} 照旧命中回落。
func TestFunctionVariantUnchangedByDispatch(t *testing.T) {
	// 现代 function 指名：无 Raw，同族编码重建同形。
	req, err := DecodeRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"alpha"}}],` +
		`"tool_choice":{"type":"function","function":{"name":"alpha"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.ToolChoice.Mode != ir.ToolChoiceTool || req.ToolChoice.Name != "alpha" {
		t.Fatalf("function 指名错：%+v", req.ToolChoice)
	}
	if len(req.ToolChoice.Raw) != 0 || req.ToolChoice.RawFamily != "" {
		t.Fatalf("function 指名不该造 Raw：Raw=%s RawFamily=%q", req.ToolChoice.Raw, req.ToolChoice.RawFamily)
	}
	out, err := EncodeRequest(req.Clone())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"tool_choice":{"type":"function","function":{"name":"alpha"}}`) {
		t.Fatalf("function 指名没重建同形：%s", out)
	}

	// 废弃扁平 {"name":"x"}（无 type）仍命中回落。
	req2, err := DecodeRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"function_call":{"name":"flat_fn"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if req2.ToolChoice == nil || req2.ToolChoice.Name != "flat_fn" {
		t.Fatalf("扁平指名未命中：%+v", req2.ToolChoice)
	}
}
