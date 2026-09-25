package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 思考配置现代化（2026）：thinking.type 增 "adaptive"（"enabled" 已标废弃，
// 模型自主决定思考量、不带预算），thinking.display 取值 summarized|omitted，
// output_config.effort 为封闭五值 low/medium/high/xhigh/max（OpenAI effort
// 七值的子集，没有 none/minimal）。
//
// 对应旧仓 #28（f2be160）。

// adaptive + display 同族往返：adaptive 原样回写、不发明预算，display 带回。
func TestAdaptiveThinkingRoundTrip(t *testing.T) {
	body := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],` +
		`"thinking":{"type":"adaptive","display":"omitted"}}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	th := req.Thinking
	if th == nil || !th.On() || !th.Adaptive {
		t.Fatalf("adaptive 未进 IR：%+v", th)
	}
	if th.Display != "omitted" {
		t.Errorf("Display = %q, want omitted", th.Display)
	}
	if th.BudgetTokens != 0 {
		t.Errorf("adaptive 不应带预算：%d", th.BudgetTokens)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"thinking":{"type":"adaptive","display":"omitted"}`) {
		t.Errorf("adaptive+display 未回写: %s", s)
	}
	if strings.Contains(s, "budget_tokens") {
		t.Errorf("adaptive 不应发明预算: %s", s)
	}
}

// enabled（旧形态）+ 预算 + display 同族往返。
func TestEnabledThinkingDisplayRoundTrip(t *testing.T) {
	body := `{"model":"m","max_tokens":9000,"messages":[{"role":"user","content":"hi"}],` +
		`"thinking":{"type":"enabled","budget_tokens":5000,"display":"summarized"}}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	th := req.Thinking
	if th == nil || th.Adaptive || th.BudgetTokens != 5000 || !th.On() {
		t.Fatalf("enabled 形态错位：%+v", th)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"type":"enabled"`) || !strings.Contains(s, `"budget_tokens":5000`) ||
		!strings.Contains(s, `"display":"summarized"`) {
		t.Errorf("enabled+预算+display 未回写: %s", s)
	}
}

// output_config.effort 封闭五值逐一往返：解码进 Thinking.Effort，编码原值回写。
func TestOutputConfigEffortRoundTrip(t *testing.T) {
	for _, lv := range []string{"low", "medium", "high", "xhigh", "max"} {
		body := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],` +
			`"thinking":{"type":"adaptive"},"output_config":{"effort":"` + lv + `"}}`
		req, err := DecodeRequest([]byte(body))
		if err != nil {
			t.Fatalf("%s DecodeRequest: %v", lv, err)
		}
		if req.Thinking == nil || req.Thinking.Effort != lv {
			t.Fatalf("%s effort 未进 IR：%+v", lv, req.Thinking)
		}
		out, err := EncodeRequest(req)
		if err != nil {
			t.Fatalf("%s EncodeRequest: %v", lv, err)
		}
		if !strings.Contains(string(out), `"effort":"`+lv+`"`) {
			t.Errorf("%s effort 未回写: %s", lv, out)
		}
	}
}

// effort 独立出现（没带 thinking 块）也算开了思考，同族往返不丢。
func TestEffortOnlyImpliesThinking(t *testing.T) {
	body := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],` +
		`"output_config":{"effort":"high"}}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if req.Thinking == nil || !req.Thinking.On() || req.Thinking.Effort != "high" {
		t.Fatalf("effort-only 未开思考：%+v", req.Thinking)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(out), `"effort":"high"`) {
		t.Errorf("effort 未回写: %s", out)
	}
}

// 值集装不下的档位（OpenAI 的 minimal、未知值）出站不写，由诊断报出；
// "none" 与未开思考同义，同样不写但无需报。
func TestEffortOutOfSetDropped(t *testing.T) {
	base := func(effort string) *ir.Request {
		return &ir.Request{
			Model: "m", MaxTokens: 4096,
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
			Thinking: &ir.ThinkingConfig{Enabled: ir.ThinkingOn(), Effort: effort, BudgetTokens: 1024},
		}
	}
	for _, lv := range []string{"minimal", "none", "turbo"} {
		out, err := EncodeRequest(base(lv))
		if err != nil {
			t.Fatalf("%s EncodeRequest: %v", lv, err)
		}
		if strings.Contains(string(out), `"effort"`) {
			t.Errorf("%q 越集仍写出: %s", lv, out)
		}
	}
}

// 全缺省时不多一个键：没有 thinking 也没有 output_config。
func TestThinkingAbsentStaysAbsent(t *testing.T) {
	out, err := EncodeRequest(&ir.Request{
		Model: "m", MaxTokens: 10,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "thinking") || strings.Contains(s, "output_config") {
		t.Errorf("缺省时发明思考配置: %s", s)
	}
}
