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
)

// 这个文件守 reasoning 子参数三维（summary / context / mode）的保真：
//   - responses 族同族往返四维俱全，不被 auto 兜底改写；
//   - 开关未表态时子参数照收照送，不发明 effort；
//   - 同一个 effort 取值从 chat 与 responses 两路入站解出同一个 IR 语义；
//   - 跨族丢弃必报——子参数轴与开/关轴独立，报了「没有推理模式」
//     不等于报了「summary 偏好丢了」。

// rpResponsesBody 造一份带指定 reasoning 对象的最小 responses 请求体。
func rpResponsesBody(reasoning string) string {
	return `{"model":"user-model","max_output_tokens":64,` +
		`"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],` +
		`"reasoning":` + reasoning + `}`
}

// rpChatBody 造一份带指定 reasoning_effort 的最小 chat 请求体。
func rpChatBody(effort string) string {
	return `{"model":"user-model","max_tokens":64,` +
		`"messages":[{"role":"user","content":"hi"}],` +
		`"reasoning_effort":` + effort + `}`
}

func rpDecode(t *testing.T, in, body string) *ir.Request {
	t.Helper()
	ic, ok := codec.Inbound(in)
	if !ok {
		t.Fatalf("inbound %q not registered", in)
	}
	req, err := ic.DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("%s decode: %v", in, err)
	}
	return req
}

func rpEncode(t *testing.T, out string, req *ir.Request) string {
	t.Helper()
	oc, ok := codec.Outbound(out)
	if !ok {
		t.Fatalf("outbound %q not registered", out)
	}
	encoded, err := oc.EncodeRequest(req)
	if err != nil {
		t.Fatalf("%s encode: %v", out, err)
	}
	return string(encoded)
}

// rpMinimal 造一份可编码的最小 IR 请求，Thinking 由调用方给。
func rpMinimal(th *ir.ThinkingConfig) *ir.Request {
	return &ir.Request{
		Model:     "native",
		MaxTokens: 64,
		Messages: []ir.Message{{Role: ir.RoleUser,
			Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		Thinking: th,
	}
}

// 四维同族往返：客户端给了 summary 就不能被 auto 兜底顶掉，
// context/mode 原文透传。
func TestResponsesReasoningSubParamsRoundTrip(t *testing.T) {
	req := rpDecode(t, codec.ProtocolResponses, rpResponsesBody(
		`{"effort":"high","summary":"detailed","context":"all_turns","mode":"pro"}`))
	th := req.Thinking
	if th == nil {
		t.Fatal("reasoning 解出来是 nil")
	}
	if !th.On() || th.Effort != "high" || th.Summary != "detailed" {
		t.Errorf("开关/档位/摘要解错: on=%v effort=%q summary=%q", th.On(), th.Effort, th.Summary)
	}
	if string(th.Context) != `"all_turns"` || string(th.Mode) != `"pro"` {
		t.Errorf("子参数原文没留住: context=%s mode=%s", th.Context, th.Mode)
	}

	text := rpEncode(t, codec.ProtocolResponses, req)
	for _, want := range []string{
		`"effort":"high"`, `"summary":"detailed"`,
		`"context":"all_turns"`, `"mode":"pro"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("出站缺 %s: %s", want, text)
		}
	}
	if strings.Contains(text, `"summary":"auto"`) {
		t.Errorf("客户端给过 summary 仍被 auto 顶掉: %s", text)
	}
}

// 开关未表态、只给了子参数：照收照送，不发明 effort——
// 补档位等于替客户端说「要思考」，而它没说过。
func TestSummaryOnlyKeepsSubParamsAndInventsNoEffort(t *testing.T) {
	req := rpDecode(t, codec.ProtocolResponses, rpResponsesBody(
		`{"summary":"detailed","context":"all_turns"}`))
	th := req.Thinking
	if th == nil {
		t.Fatal("只有子参数时 reasoning 该解出非 nil")
	}
	if th.Enabled != nil {
		t.Errorf("开关该保持未表态，实际 %v", *th.Enabled)
	}
	if th.Summary != "detailed" || string(th.Context) != `"all_turns"` {
		t.Errorf("子参数没收进来: summary=%q context=%s", th.Summary, th.Context)
	}

	text := rpEncode(t, codec.ProtocolResponses, req)
	if !strings.Contains(text, `"summary":"detailed"`) ||
		!strings.Contains(text, `"context":"all_turns"`) {
		t.Errorf("子参数没送出去: %s", text)
	}
	if strings.Contains(text, `"effort"`) {
		t.Errorf("发明了客户端没给的 effort: %s", text)
	}
}

// 显式 null 与缺省同义：不能把 "null" 字面量当值透传上送。
func TestExplicitNullSubParamsStayAbsent(t *testing.T) {
	req := rpDecode(t, codec.ProtocolResponses, rpResponsesBody(
		`{"effort":"high","context":null,"mode":null}`))
	if len(req.Thinking.Context) > 0 || len(req.Thinking.Mode) > 0 {
		t.Fatalf("null 该归零: context=%s mode=%s", req.Thinking.Context, req.Thinking.Mode)
	}
	text := rpEncode(t, codec.ProtocolResponses, req)
	if strings.Contains(text, `"context"`) || strings.Contains(text, `"mode"`) {
		t.Errorf("null 子参数上了线: %s", text)
	}
}

// 同一个 effort 取值，两路入站必须解出同一个语义：
// 否则同一家客户端换协议接入会静默换行为。
func TestEffortParityAcrossFamilies(t *testing.T) {
	for _, effort := range []string{"none", "minimal", "low", "medium", "high"} {
		fromChat := rpDecode(t, codec.ProtocolChatCompletions, rpChatBody(`"`+effort+`"`)).Thinking
		fromResp := rpDecode(t, codec.ProtocolResponses, rpResponsesBody(`{"effort":"`+effort+`"}`)).Thinking
		if fromChat == nil || fromResp == nil {
			t.Fatalf("%s: 有一路解出 nil: chat=%v resp=%v", effort, fromChat, fromResp)
		}
		if (fromChat.Enabled == nil) != (fromResp.Enabled == nil) ||
			(fromChat.Enabled != nil && *fromChat.Enabled != *fromResp.Enabled) {
			t.Errorf("%s: 开关不一致 chat=%v resp=%v", effort, fromChat.Enabled, fromResp.Enabled)
		}
		if fromChat.Effort != fromResp.Effort {
			t.Errorf("%s: 档位不一致 chat=%q resp=%q", effort, fromChat.Effort, fromResp.Effort)
		}
	}
}

// 明确关闭要落到线上，且关闭时不索要推理签名：
// 没有推理内容可签，带着 include 是自相矛盾的请求。
func TestExplicitNoneReachesWireBothFamilies(t *testing.T) {
	req := rpMinimal(&ir.ThinkingConfig{Enabled: ir.ThinkingOff()})

	text := rpEncode(t, codec.ProtocolChatCompletions, req)
	if !strings.Contains(text, `"reasoning_effort":"none"`) {
		t.Errorf("chat 出站没写明确关闭: %s", text)
	}

	text = rpEncode(t, codec.ProtocolResponses, req)
	if !strings.Contains(text, `"effort":"none"`) {
		t.Errorf("responses 出站没写明确关闭: %s", text)
	}
	if strings.Contains(text, "encrypted_content") {
		t.Errorf("关闭推理仍索要签名: %s", text)
	}
}

// 开了思考但没给档位：responses 补 medium + summary auto + 索要签名。
// 签名是推理跨轮接续的唯一载体，摘要不能回传。
func TestOnWithoutEffortGetsMediumAutoAndSig(t *testing.T) {
	req := rpMinimal(&ir.ThinkingConfig{Enabled: ir.ThinkingOn()})
	text := rpEncode(t, codec.ProtocolResponses, req)
	for _, want := range []string{
		`"effort":"medium"`, `"summary":"auto"`, `"reasoning.encrypted_content"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("出站缺 %s: %s", want, text)
		}
	}
}

// 跨族丢弃必报：子参数三维只有 responses 族有线格。
// 报「没有推理模式」不等于报「summary 偏好丢了」，两轴独立各自成立。
func TestSubParamLossNotesPerTarget(t *testing.T) {
	req := rpMinimal(&ir.ThinkingConfig{
		Enabled: ir.ThinkingOn(), Effort: "high",
		Summary: "detailed",
		Context: json.RawMessage(`"all_turns"`),
		Mode:    json.RawMessage(`"pro"`),
	})

	for _, target := range []string{
		codec.ProtocolChatCompletions, codec.ProtocolAnthropic, codec.ProtocolGemini,
	} {
		notes := lossyFor(t, target, req)
		for _, want := range []string{
			"reasoning summary preference", "reasoning context scope", "reasoning mode",
		} {
			if !strings.Contains(notes, want) {
				t.Errorf("%s 缺 %q 的注记: %s", target, want, notes)
			}
		}
	}

	if notes := lossyFor(t, codec.ProtocolResponses, req); notes != "" &&
		strings.Contains(notes, "reasoning ") {
		t.Errorf("responses 族有槽位却报了丢弃: %s", notes)
	}

	// 开关未表态、只有 summary：报的是子参数轴，不能因为
	// 「没说开思考」就静默——客户端的偏好照样丢了。
	only := rpMinimal(&ir.ThinkingConfig{Summary: "concise"})
	if notes := lossyFor(t, codec.ProtocolChatCompletions, only); !strings.Contains(notes, "reasoning summary preference") {
		t.Errorf("仅 summary 也该报: %s", notes)
	}
}
