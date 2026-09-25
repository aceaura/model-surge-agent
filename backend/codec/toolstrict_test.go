package codec_test

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	_ "github.com/aceaura/model-surge-agent/backend/codec/anthropic"
	_ "github.com/aceaura/model-surge-agent/backend/codec/chatcompletions"
	_ "github.com/aceaura/model-surge-agent/backend/codec/gemini"
	_ "github.com/aceaura/model-surge-agent/backend/codec/responses"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 工具 strict（schema 严格校验保证）三族贯通：anthropic tool.strict、
// chat_completions function.strict、responses FunctionTool.strict 同义同形；
// gemini 的工具定义没有这一维，出站不写、诊断报数不报值。
// 三态指针：显式 false 与没给语义不同，都必须保真。
//
// 对应旧仓 #26（ca016e7）。

func strictPtr(b bool) *bool { return &b }

func TestToolStrictDecodeThreeFamilies(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{codec.ProtocolAnthropic, `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],` +
			`"tools":[{"name":"ping","input_schema":{"type":"object"},"strict":true},` +
			`{"name":"pong","input_schema":{"type":"object"},"strict":false},` +
			`{"name":"plain","input_schema":{"type":"object"}}]}`},
		{codec.ProtocolChatCompletions, `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
			`"tools":[{"type":"function","function":{"name":"ping","strict":true}},` +
			`{"type":"function","function":{"name":"pong","strict":false}},` +
			`{"type":"function","function":{"name":"plain"}}]}`},
		{codec.ProtocolResponses, `{"model":"m","input":"hi",` +
			`"tools":[{"type":"function","name":"ping","strict":true},` +
			`{"type":"function","name":"pong","strict":false},` +
			`{"type":"function","name":"plain"}]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			ic, ok := codec.Inbound(c.name)
			if !ok {
				t.Fatalf("inbound %q not registered", c.name)
			}
			req, err := ic.DecodeRequest([]byte(c.body))
			if err != nil {
				t.Fatalf("DecodeRequest: %v", err)
			}
			if len(req.Tools) != 3 {
				t.Fatalf("tools = %d, want 3", len(req.Tools))
			}
			if req.Tools[0].Strict == nil || !*req.Tools[0].Strict {
				t.Errorf("ping strict 应解出 true：%+v", req.Tools[0].Strict)
			}
			if req.Tools[1].Strict == nil || *req.Tools[1].Strict {
				t.Errorf("pong 显式 false 应保真（非 nil）：%+v", req.Tools[1].Strict)
			}
			if req.Tools[2].Strict != nil {
				t.Errorf("plain 没给应为 nil：%+v", req.Tools[2].Strict)
			}
		})
	}
}

// 同族与跨族回写：true/false 都回写，nil 不发明键。
func TestToolStrictRoundTrip(t *testing.T) {
	req := &ir.Request{
		Model: "m", MaxTokens: 100,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		Tools: []ir.Tool{
			{Name: "ping", Schema: `{"type":"object"}`, Strict: strictPtr(true)},
			{Name: "pong", Schema: `{"type":"object"}`, Strict: strictPtr(false)},
			{Name: "plain", Schema: `{"type":"object"}`},
		},
	}
	for _, name := range []string{codec.ProtocolAnthropic, codec.ProtocolChatCompletions, codec.ProtocolResponses} {
		t.Run(name, func(t *testing.T) {
			oc, ok := codec.Outbound(name)
			if !ok {
				t.Fatalf("outbound %q not registered", name)
			}
			out, err := oc.EncodeRequest(req)
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			body := string(out)
			if !strings.Contains(body, `"strict":true`) || !strings.Contains(body, `"strict":false`) {
				t.Errorf("strict 三态回写缺失: %s", body)
			}
			if strings.Count(body, `"strict"`) != 2 {
				t.Errorf("nil 不应发明 strict 键: %s", body)
			}
		})
	}
}

// 跨族到 gemini：strict 一个字符都不进载荷（丢的部分由诊断报出）。
func TestToolStrictNeverLeaksToGemini(t *testing.T) {
	req := &ir.Request{
		Model: "m", MaxTokens: 100,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		Tools:    []ir.Tool{{Name: "ping", Schema: `{"type":"object"}`, Strict: strictPtr(true)}},
	}
	oc, ok := codec.Outbound(codec.ProtocolGemini)
	if !ok {
		t.Fatal("gemini outbound not registered")
	}
	out, err := oc.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if strings.Contains(string(out), "strict") {
		t.Errorf("gemini 泄漏 strict: %s", out)
	}
}

// 诊断：三族有槽位静默，gemini 无槽位报数不报值；nil（没给）不计数。
func TestDiagnoseToolStrictDroppedToGemini(t *testing.T) {
	req := &ir.Request{
		Model: "m", MaxTokens: 10, Tools: []ir.Tool{
			{Name: "a", Strict: strictPtr(true)},
			{Name: "b", Strict: strictPtr(false)},
			{Name: "c"}, // nil 不计
		},
	}
	oc, _ := codec.Outbound(codec.ProtocolGemini)
	got := strings.Join(codec.DescribeLossy(req, codec.ProtocolGemini, oc.Caps()), "; ")
	if !strings.Contains(got, "strict flag on 2 tool(s)") {
		t.Errorf("gemini 丢弃 strict 未报告：%q", got)
	}
	for _, name := range []string{codec.ProtocolAnthropic, codec.ProtocolChatCompletions, codec.ProtocolResponses} {
		oc, _ := codec.Outbound(name)
		if notes := codec.DescribeLossy(req, name, oc.Caps()); len(notes) != 0 {
			t.Errorf("%s 接得住 strict，误报：%v", name, notes)
		}
	}
}
