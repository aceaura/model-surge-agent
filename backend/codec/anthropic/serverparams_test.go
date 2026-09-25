package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 服务端工具声明参数（官方 WebSearchTool20250305 的 max_uses/
// allowed_domains/blocked_domains/user_location）此前 wireTool 一个槽位
// 都没有：同族往返静默丢，客户端要的限制不生效也无从知晓。#74 的原文
// 通道盖住「入站来的」往返，本组钉住结构化视图与「内部构造（无原文）」
// 的可编码性。

// 解码：四维收进 ServerParams 结构化视图，与 ServerRaw 并存；
// 一个参数都没给的工具保持 nil（缺省就是缺省）。
func TestServerToolParamsDecodeStructured(t *testing.T) {
	body := []byte(`{"model":"claude","max_tokens":64,"messages":[{"role":"user","content":"hi"}],` +
		`"tools":[` +
		`{"type":"web_search_20250305","name":"web_search","max_uses":3,` +
		`"allowed_domains":["a.com","b.com"],"blocked_domains":["c.com"],` +
		`"user_location":{"type":"approximate","city":"Paris"}},` +
		`{"type":"code_execution_20250825","name":"code_execution"}` +
		`]}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Tools) != 2 {
		t.Fatalf("工具没解出来：%+v", req.Tools)
	}
	p := req.Tools[0].ServerParams
	if p == nil {
		t.Fatalf("声明参数没收下：%+v", req.Tools[0])
	}
	if p.MaxUses != 3 || len(p.AllowedDomains) != 2 || p.AllowedDomains[0] != "a.com" ||
		len(p.BlockedDomains) != 1 || p.BlockedDomains[0] != "c.com" ||
		!strings.Contains(string(p.UserLocation), `"city":"Paris"`) {
		t.Errorf("参数结构化错：%+v", p)
	}
	if p.SearchContextSize != "" {
		t.Errorf("anthropic 线体没有 search_context_size，不得凭空出现：%+v", p)
	}
	// 原文通道与结构化视图并存：原文负责同族字节级回吐，结构负责观测面。
	if len(req.Tools[0].ServerRaw) == 0 {
		t.Errorf("ServerRaw 丢了：%+v", req.Tools[0])
	}
	if req.Tools[1].ServerParams != nil {
		t.Errorf("没给参数的工具应保持 nil：%+v", req.Tools[1].ServerParams)
	}
}

// 编码：内部构造（无原文）的服务端工具靠 ServerParams 把参数编上线，
// user_location 原文透传。
func TestServerToolParamsEncodeWithoutRaw(t *testing.T) {
	req := &ir.Request{
		Model: "claude", MaxTokens: 64,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		Tools: []ir.Tool{{
			Name: "web_search", ServerType: "web_search_20250305",
			ServerParams: &ir.ServerParams{
				MaxUses:        2,
				AllowedDomains: []string{"a.com"},
				BlockedDomains: []string{"c.com"},
				UserLocation:   []byte(`{"type":"approximate","city":"Lyon"}`),
			},
		}},
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		`"type":"web_search_20250305"`, `"max_uses":2`,
		`"allowed_domains":["a.com"]`, `"blocked_domains":["c.com"]`,
		`"user_location":{"type":"approximate","city":"Lyon"}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("声明参数没编上线 %s：%s", want, s)
		}
	}
	if strings.Contains(s, "input_schema") {
		t.Errorf("服务端工具混进了参数形状：%s", s)
	}
}

// 缺省保持缺省：ServerParams 为 nil 时一个键也不造——空 max_uses/空数组
// 发出去会改变上游行为（0 次调用上限、空白名单全禁）。
func TestServerToolParamsAbsentNotInvented(t *testing.T) {
	req := &ir.Request{
		Model: "claude", MaxTokens: 64,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		Tools:    []ir.Tool{{Name: "web_search", ServerType: "web_search_20250305"}},
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, unwanted := range []string{"max_uses", "allowed_domains", "blocked_domains", "user_location"} {
		if strings.Contains(s, unwanted) {
			t.Errorf("没给的参数被凭空造出 %s：%s", unwanted, s)
		}
	}
}

// 函数工具不吃参数通道：这些键在函数工具线体上不存在，写出去是非法形状。
func TestServerToolParamsNotForFunctionTools(t *testing.T) {
	req := &ir.Request{
		Model: "claude", MaxTokens: 64,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		Tools: []ir.Tool{{
			Name: "f1", Schema: `{"type":"object"}`,
			ServerParams: &ir.ServerParams{MaxUses: 9, AllowedDomains: []string{"x.com"}},
		}},
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, `"max_uses":9`) || strings.Contains(s, "x.com") {
		t.Errorf("函数工具吃了声明参数：%s", s)
	}
	if !strings.Contains(s, `"input_schema":{"type":"object"}`) {
		t.Errorf("函数工具正常形状丢了：%s", s)
	}
}

// 有原文时原文优先：整块回吐已经字节级保真，结构化参数不得再叠出重复键。
func TestServerToolParamsRawTakesPrecedence(t *testing.T) {
	body := []byte(`{"model":"claude","max_tokens":64,"messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":5}]}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.Tools[0].ServerParams == nil || req.Tools[0].ServerParams.MaxUses != 5 {
		t.Fatalf("结构化参数没收下：%+v", req.Tools[0].ServerParams)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(out), `"max_uses"`); n != 1 {
		t.Errorf("max_uses 出现 %d 次，原文回吐不得叠键：%s", n, out)
	}
}
