package codec_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	_ "github.com/aceaura/model-surge-agent/backend/codec/anthropic"
	_ "github.com/aceaura/model-surge-agent/backend/codec/chatcompletions"
	_ "github.com/aceaura/model-surge-agent/backend/codec/gemini"
	_ "github.com/aceaura/model-surge-agent/backend/codec/responses"
	"github.com/aceaura/model-surge-agent/backend/ir"
	"github.com/aceaura/model-surge-agent/backend/paramover"
)

// 这个文件守的是推理开关的三态：客户端没提、明确开启、明确关闭。
// 三态必须分开，因为只有「明确关闭」该在出站写出关闭标记；
// 「没提」写出任何标记都等于替客户端做决定。

// disableFixture 是各入站协议表达「明确关闭推理」的写法。
var disableFixture = map[string]string{
	codec.ProtocolAnthropic:       `{"model":"m","max_tokens":64,"thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hi"}]}`,
	codec.ProtocolChatCompletions: `{"model":"m","reasoning_effort":"none","messages":[{"role":"user","content":"hi"}]}`,
	codec.ProtocolResponses:       `{"model":"m","reasoning":{"effort":"none"},"input":"hi"}`,
}

// silentFixture 是同样的请求但完全不提推理。
var silentFixture = map[string]string{
	codec.ProtocolAnthropic:       `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`,
	codec.ProtocolChatCompletions: `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
	codec.ProtocolResponses:       `{"model":"m","input":"hi"}`,
}

func decodeReq(t *testing.T, proto, body string) *ir.Request {
	t.Helper()
	in, ok := codec.Inbound(proto)
	if !ok {
		t.Fatalf("no inbound codec for %s", proto)
	}
	req, err := in.DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("%s decode: %v", proto, err)
	}
	return req
}

func TestInboundDecodesExplicitDisable(t *testing.T) {
	for proto, body := range disableFixture {
		t.Run(proto, func(t *testing.T) {
			req := decodeReq(t, proto, body)
			if req.Thinking == nil {
				t.Fatal("明确关闭必须留下痕迹，Thinking 不能为 nil")
			}
			if !req.Thinking.Off() {
				t.Errorf("want Off(), got Enabled=%v", req.Thinking.Enabled)
			}
			if req.Thinking.On() {
				t.Error("明确关闭不能同时判为开启")
			}
		})
	}
}

func TestInboundLeavesSilenceUnset(t *testing.T) {
	for proto, body := range silentFixture {
		t.Run(proto, func(t *testing.T) {
			req := decodeReq(t, proto, body)
			// 没提就不该被解成任何一种表态：解成关闭会让出站写出关闭标记，
			// 篡改上游默认；解成开启则凭空要求推理。
			if req.Thinking.On() || req.Thinking.Off() {
				t.Errorf("没提推理却解出了表态: %+v", req.Thinking)
			}
		})
	}
}

// disableMarker 断言某出站协议的请求体表达了「关闭」。
func assertDisabled(t *testing.T, proto string, body []byte) {
	t.Helper()
	var w map[string]any
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	switch proto {
	case codec.ProtocolAnthropic:
		th, ok := w["thinking"].(map[string]any)
		if !ok {
			t.Fatalf("anthropic: 缺 thinking 键: %s", body)
		}
		if th["type"] != "disabled" {
			t.Errorf("anthropic: thinking.type = %v, want disabled", th["type"])
		}
	case codec.ProtocolChatCompletions:
		if w["reasoning_effort"] != "none" {
			t.Errorf("chat_completions: reasoning_effort = %v, want none", w["reasoning_effort"])
		}
	case codec.ProtocolResponses:
		r, ok := w["reasoning"].(map[string]any)
		if !ok {
			t.Fatalf("responses: 缺 reasoning 键: %s", body)
		}
		if r["effort"] != "none" {
			t.Errorf("responses: reasoning.effort = %v, want none", r["effort"])
		}
		if _, has := r["summary"]; has {
			t.Errorf("responses: 关闭时不该请求 summary: %s", body)
		}
	case codec.ProtocolGemini:
		cfg, ok := w["generationConfig"].(map[string]any)
		if !ok {
			t.Fatalf("gemini: 缺 generationConfig: %s", body)
		}
		tc, ok := cfg["thinkingConfig"].(map[string]any)
		if !ok {
			t.Fatalf("gemini: 缺 thinkingConfig: %s", body)
		}
		budget, ok := tc["thinkingBudget"].(float64)
		if !ok || budget != 0 {
			t.Errorf("gemini: thinkingBudget = %v, want 0", tc["thinkingBudget"])
		}
		if tc["includeThoughts"] == true {
			t.Errorf("gemini: 关闭推理时不该要求返回思考内容: %s", body)
		}
	default:
		t.Fatalf("未知协议 %s——新增出站协议必须在这里给出关闭断言", proto)
	}
}

// assertNoThinkingMention 断言请求体里完全没有推理相关键。
func assertNoThinkingMention(t *testing.T, proto string, body []byte) {
	t.Helper()
	var w map[string]any
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	switch proto {
	case codec.ProtocolAnthropic:
		if _, has := w["thinking"]; has {
			t.Errorf("anthropic: 客户端没提却写出了 thinking: %s", body)
		}
	case codec.ProtocolChatCompletions:
		if _, has := w["reasoning_effort"]; has {
			t.Errorf("chat_completions: 客户端没提却写出了 reasoning_effort: %s", body)
		}
	case codec.ProtocolResponses:
		if _, has := w["reasoning"]; has {
			t.Errorf("responses: 客户端没提却写出了 reasoning: %s", body)
		}
	case codec.ProtocolGemini:
		cfg, _ := w["generationConfig"].(map[string]any)
		if cfg != nil {
			if _, has := cfg["thinkingConfig"]; has {
				t.Errorf("gemini: 客户端没提却写出了 thinkingConfig: %s", body)
			}
		}
	default:
		t.Fatalf("未知协议 %s", proto)
	}
}

func encodeOut(t *testing.T, proto string, req *ir.Request) []byte {
	t.Helper()
	out, ok := codec.Outbound(proto)
	if !ok {
		t.Fatalf("no outbound codec for %s", proto)
	}
	body, err := out.EncodeRequest(req)
	if err != nil {
		t.Fatalf("%s encode: %v", proto, err)
	}
	return body
}

func baseReq() *ir.Request {
	return &ir.Request{
		Model:     "m",
		MaxTokens: 64,
		Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}
}

func TestOutboundWritesExplicitDisable(t *testing.T) {
	for _, proto := range outboundNames() {
		t.Run(proto, func(t *testing.T) {
			req := baseReq()
			req.Thinking = &ir.ThinkingConfig{Enabled: ir.ThinkingOff()}
			assertDisabled(t, proto, encodeOut(t, proto, req))
		})
	}
}

func TestOutboundOmitsThinkingWhenClientSilent(t *testing.T) {
	for _, proto := range outboundNames() {
		t.Run(proto, func(t *testing.T) {
			assertNoThinkingMention(t, proto, encodeOut(t, proto, baseReq()))
		})
	}
}

// TestOutboundDisableIsNotFoldedIntoAnEffort 守的是那条最阴的路径：
// 关闭态若被当成「没有档位的开启态」，出站会用 effortForBudget 折出一个
// 真实档位，请求就从关闭变成了开启。
func TestOutboundDisableDoesNotBecomeEnabled(t *testing.T) {
	for _, proto := range outboundNames() {
		t.Run(proto, func(t *testing.T) {
			req := baseReq()
			req.Thinking = &ir.ThinkingConfig{Enabled: ir.ThinkingOff()}
			body := string(encodeOut(t, proto, req))
			for _, bad := range []string{`"type":"enabled"`, `"effort":"low"`, `"effort":"medium"`, `"effort":"high"`, `"includeThoughts":true`} {
				if strings.Contains(body, bad) {
					t.Errorf("关闭请求里出现了开启标记 %s: %s", bad, body)
				}
			}
		})
	}
}

// TestDisableSurvivesEveryProtocolPair 是三入站 × 四出站的关闭态矩阵：
// 明确关闭在任一对协议之间都必须仍然是关闭。
func TestDisableSurvivesEveryProtocolPair(t *testing.T) {
	for _, in := range inboundNames() {
		body, ok := disableFixture[in]
		if !ok {
			t.Fatalf("入站协议 %s 没有关闭态 fixture——新增入站协议必须补上", in)
		}
		for _, out := range outboundNames() {
			t.Run(in+"→"+out, func(t *testing.T) {
				req := decodeReq(t, in, body)
				assertDisabled(t, out, encodeOut(t, out, req))
			})
		}
	}
}

// TestSilenceSurvivesEveryProtocolPair 是同一张矩阵的「没提」那一格。
func TestSilenceSurvivesEveryProtocolPair(t *testing.T) {
	for _, in := range inboundNames() {
		body, ok := silentFixture[in]
		if !ok {
			t.Fatalf("入站协议 %s 没有静默 fixture", in)
		}
		for _, out := range outboundNames() {
			t.Run(in+"→"+out, func(t *testing.T) {
				req := decodeReq(t, in, body)
				assertNoThinkingMention(t, out, encodeOut(t, out, req))
			})
		}
	}
}

// TestDefaultsDoNotFlipExplicitDisable 是与参数覆盖层的交互：
// 运维在模型配置里填了「默认开推理」，而客户端明确要求关闭，客户端赢。
// 这一条只有出站把关闭写成显式键才成立——defaults 是「缺失才填」。
func TestDefaultsDoNotFlipExplicitDisable(t *testing.T) {
	cases := map[string]string{
		codec.ProtocolAnthropic:       `{"thinking":{"type":"enabled","budget_tokens":8192}}`,
		codec.ProtocolChatCompletions: `{"reasoning_effort":"high"}`,
		codec.ProtocolResponses:       `{"reasoning":{"effort":"high"}}`,
		codec.ProtocolGemini:          `{"generationConfig":{"thinkingConfig":{"thinkingBudget":8192}}}`,
	}
	for _, proto := range outboundNames() {
		defaults, ok := cases[proto]
		if !ok {
			t.Fatalf("出站协议 %s 缺 defaults fixture", proto)
		}
		t.Run(proto, func(t *testing.T) {
			req := baseReq()
			req.Thinking = &ir.ThinkingConfig{Enabled: ir.ThinkingOff()}
			merged, _, err := paramover.Apply(encodeOut(t, proto, req), []byte(defaults), nil)
			if err != nil {
				t.Fatalf("paramover: %v", err)
			}
			assertDisabled(t, proto, merged)
		})
	}
}

// TestDefaultsStillApplyWhenClientSilent 是上一条的对照：没提的时候
// defaults 必须照常生效，否则修 bug 修成了把参数层整个废掉。
func TestDefaultsStillApplyWhenClientSilent(t *testing.T) {
	body := encodeOut(t, codec.ProtocolAnthropic, baseReq())
	merged, _, err := paramover.Apply(body, []byte(`{"thinking":{"type":"enabled","budget_tokens":8192}}`), nil)
	if err != nil {
		t.Fatalf("paramover: %v", err)
	}
	var w map[string]any
	if err := json.Unmarshal(merged, &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	th, ok := w["thinking"].(map[string]any)
	if !ok || th["type"] != "enabled" {
		t.Errorf("客户端没提时 defaults 必须生效: %s", merged)
	}
}

// TestOverridesStillBeatExplicitDisable：overrides 是强制压盖层，
// 运维用它就是要无条件生效，客户端的明确关闭也压得住。
func TestOverridesStillBeatExplicitDisable(t *testing.T) {
	req := baseReq()
	req.Thinking = &ir.ThinkingConfig{Enabled: ir.ThinkingOff()}
	merged, _, err := paramover.Apply(encodeOut(t, codec.ProtocolAnthropic, req), nil,
		[]byte(`{"thinking":{"type":"enabled","budget_tokens":8192}}`))
	if err != nil {
		t.Fatalf("paramover: %v", err)
	}
	var w map[string]any
	if err := json.Unmarshal(merged, &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	th, _ := w["thinking"].(map[string]any)
	if th == nil || th["type"] != "enabled" {
		t.Errorf("overrides 必须压过客户端: %s", merged)
	}
}

// TestLossyIgnoresExplicitDisable：出站不支持推理时，客户端要求关闭
// 恰好就是结果本身，报「丢了 thinking」是假警报。
func TestLossyIgnoresExplicitDisable(t *testing.T) {
	req := baseReq()
	req.Thinking = &ir.ThinkingConfig{Enabled: ir.ThinkingOff()}
	if mentionsThinking(codec.DescribeLossy(req, "toy", codec.Capabilities{})) {
		t.Error("明确关闭不该报有损")
	}

	req.Thinking = &ir.ThinkingConfig{Enabled: ir.ThinkingOn(), BudgetTokens: 1024}
	if !mentionsThinking(codec.DescribeLossy(req, "toy", codec.Capabilities{})) {
		t.Error("明确开启遇上不支持推理的协议必须报有损")
	}
}

func mentionsThinking(notes []string) bool {
	for _, n := range notes {
		if strings.Contains(n, "thinking") {
			return true
		}
	}
	return false
}

// TestOutboundIgnoresUnstatedThinking：结构在但没表态时，四个出站都不该
// 写出任何推理键。等价于「客户端没提」。
func TestOutboundIgnoresUnstatedThinking(t *testing.T) {
	for _, proto := range outboundNames() {
		t.Run(proto, func(t *testing.T) {
			req := baseReq()
			req.Thinking = &ir.ThinkingConfig{BudgetTokens: 1024, Effort: "high"}
			assertNoThinkingMention(t, proto, encodeOut(t, proto, req))
		})
	}
}
