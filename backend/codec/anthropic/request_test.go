package anthropic

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

const fullRequest = `{
  "model": "claude-opus-5",
  "max_tokens": 8192,
  "system": [{"type":"text","text":"be terse","cache_control":{"type":"ephemeral"}}],
  "messages": [
    {"role":"user","content":[
      {"type":"text","text":"look at this"},
      {"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}
    ]},
    {"role":"assistant","content":[
      {"type":"thinking","thinking":"hmm","signature":"sig-abc"},
      {"type":"tool_use","id":"tu_1","name":"grep","input":{"pattern":"x"}}
    ]},
    {"role":"user","content":[
      {"type":"tool_result","tool_use_id":"tu_1","content":[{"type":"text","text":"3 hits"}],"is_error":false}
    ]}
  ],
  "tools": [{"name":"grep","description":"search","input_schema":{"type":"object"}}],
  "tool_choice": {"type":"tool","name":"grep"},
  "temperature": 0.3,
  "top_p": 0.9,
  "top_k": 40,
  "stop_sequences": ["STOP"],
  "stream": true,
  "thinking": {"type":"enabled","budget_tokens":4096},
  "metadata": {"user_id":"u-1"}
}`

func decodeFull(t *testing.T) *ir.Request {
	t.Helper()
	req, err := DecodeRequest([]byte(fullRequest))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	return req
}

func TestDecodeRequestCarriesEveryField(t *testing.T) {
	req := decodeFull(t)

	if req.Model != "claude-opus-5" || req.MaxTokens != 8192 || !req.Stream {
		t.Fatalf("scalars lost: %+v", req)
	}
	if *req.Temperature != 0.3 || *req.TopP != 0.9 || *req.TopK != 40 {
		t.Errorf("sampling params lost: %+v", req)
	}
	if !reflect.DeepEqual(req.StopSequences, []string{"STOP"}) {
		t.Errorf("stop_sequences = %v", req.StopSequences)
	}
	if len(req.System) != 1 || req.System[0].Text != "be terse" || req.System[0].CacheCtl != "ephemeral" {
		t.Errorf("system = %+v", req.System)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d", len(req.Messages))
	}
	if img := req.Messages[0].Content[1].Media; img == nil || img.MediaType != "image/png" || img.Data != "AAAA" {
		t.Errorf("image lost: %+v", req.Messages[0].Content[1])
	}
	th := req.Messages[1].Content[0].Thinking
	if th == nil || th.Text != "hmm" || th.Signature != "sig-abc" || th.SignatureFrom != Name {
		t.Errorf("thinking = %+v", th)
	}
	tu := req.Messages[1].Content[1].ToolUse
	if tu == nil || tu.ID != "tu_1" || tu.Name != "grep" || tu.Input != `{"pattern":"x"}` {
		t.Errorf("tool_use = %+v", tu)
	}
	tr := req.Messages[2].Content[0].ToolResult
	if tr == nil || tr.ToolUseID != "tu_1" || len(tr.Content) != 1 || tr.Content[0].Text != "3 hits" {
		t.Errorf("tool_result = %+v", tr)
	}
	if len(req.Tools) != 1 || req.Tools[0].Schema != `{"type":"object"}` {
		t.Errorf("tools = %+v", req.Tools)
	}
	if req.ToolChoice == nil || req.ToolChoice.Mode != ir.ToolChoiceTool || req.ToolChoice.Name != "grep" {
		t.Errorf("tool_choice = %+v", req.ToolChoice)
	}
	if req.Thinking == nil || !req.Thinking.Enabled || req.Thinking.BudgetTokens != 4096 {
		t.Errorf("thinking config = %+v", req.Thinking)
	}
	if req.Metadata["user_id"] != "u-1" {
		t.Errorf("metadata = %v", req.Metadata)
	}
}

// 同协议往返：语义必须等价。断言在 IR 层做——把编码结果再解一次，
// 两个 IR 应当相同。直接比字节会被字段顺序与默认值补全干扰。
func TestRoundTripPreservesSemantics(t *testing.T) {
	first := decodeFull(t)
	wire, err := EncodeRequest(first)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	second, err := DecodeRequest(wire)
	if err != nil {
		t.Fatalf("DecodeRequest(round 2): %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("round trip drifted:\n first = %+v\nsecond = %+v", first, second)
	}
}

// 同协议不允许字节透传：编码结果必须是重新构造的规范形态，
// 而不是原始字节。用冗余写法被归一化来证明确实走了 IR。
func TestSameProtocolStillNormalizes(t *testing.T) {
	// content 用字符串简写、system 用字符串、缺 stream、块顺序里带 redacted_thinking。
	const sloppy = `{
      "model":"claude-opus-5",
      "system":"be terse",
      "messages":[
        {"role":"user","content":"hi"},
        {"role":"assistant","content":[{"type":"redacted_thinking","data":"opaque"},{"type":"text","text":"ok"}]}
      ]
    }`
	req, err := DecodeRequest([]byte(sloppy))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	wire, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(got["system"]) != `[{"type":"text","text":"be terse"}]` {
		t.Errorf("system must be normalized to a block array, got %s", got["system"])
	}
	if !strings.Contains(string(got["messages"]), `"content":[{"type":"text","text":"hi"}]`) {
		t.Errorf("string content must be normalized to a block array, got %s", got["messages"])
	}
	if strings.Contains(string(wire), "opaque") {
		t.Errorf("redacted_thinking must be dropped, not passed through: %s", wire)
	}
	if !strings.Contains(string(wire), `"max_tokens":4096`) {
		t.Errorf("max_tokens must be filled in, got %s", wire)
	}
}

// 对上游一律流式：客户端要不要流由数据面决定，出站请求体固定 stream:true。
func TestEncodeRequestAlwaysStreams(t *testing.T) {
	req := decodeFull(t)
	req.Stream = false
	wire, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(wire), `"stream":true`) {
		t.Fatalf("outbound request must always stream: %s", wire)
	}
}

// 跨族签名必须丢弃：把别家的 signature 发给 Anthropic 会被拒。
func TestEncodeDropsForeignThinkingSignature(t *testing.T) {
	req := &ir.Request{
		Model: "claude-opus-5",
		Messages: []ir.Message{{Role: ir.RoleAssistant, Content: []ir.Block{{
			Type:     ir.BlockThinking,
			Thinking: &ir.Thinking{Text: "hmm", Signature: "enc-xyz", SignatureFrom: codec.ProtocolResponses},
		}}}},
	}
	wire, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if strings.Contains(string(wire), "enc-xyz") {
		t.Fatalf("foreign signature leaked: %s", wire)
	}
	if !strings.Contains(string(wire), `"thinking":"hmm"`) {
		t.Fatalf("thinking text should survive without its signature: %s", wire)
	}
}

// 只给 effort 的客户端（responses/gemini 风格）转到 Anthropic 时必须补出预算，
// 否则上游会因缺 budget_tokens 拒绝。
func TestEncodeDerivesBudgetFromEffort(t *testing.T) {
	cases := []struct {
		name   string
		effort string
		max    int
		want   int
	}{
		{"low", "low", 8192, 1638},
		{"medium", "medium", 8192, 4096},
		{"high", "high", 8192, 6553},
		{"unset defaults to medium", "", 8192, 4096},
		{"raised to the 1024 floor", "low", 2048, 1024},
		{"kept under max_tokens", "high", 1024, 1023},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wire, err := EncodeRequest(&ir.Request{
				Model:     "claude-opus-5",
				MaxTokens: c.max,
				Thinking:  &ir.ThinkingConfig{Enabled: true, Effort: c.effort},
			})
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			var got wireRequest
			if err := json.Unmarshal(wire, &got); err != nil {
				t.Fatal(err)
			}
			if got.Thinking == nil || got.Thinking.BudgetTokens != c.want {
				t.Fatalf("budget = %+v, want %d", got.Thinking, c.want)
			}
			if got.Thinking.BudgetTokens >= got.MaxTokens {
				t.Fatalf("budget %d must stay under max_tokens %d", got.Thinking.BudgetTokens, got.MaxTokens)
			}
		})
	}
}

// 流被掐断时工具入参可能是残缺 JSON。发出去必须是合法对象，
// 否则整个请求会被上游按语法错误拒掉。
func TestEncodeRepairsTruncatedToolInput(t *testing.T) {
	wire, err := EncodeRequest(&ir.Request{
		Model: "claude-opus-5",
		Messages: []ir.Message{{Role: ir.RoleAssistant, Content: []ir.Block{{
			Type:    ir.BlockToolUse,
			ToolUse: &ir.ToolUse{ID: "tu_1", Name: "grep", Input: `{"pattern":"x`},
		}}}},
	})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(wire), `"input":{}`) {
		t.Fatalf("truncated input must become an empty object: %s", wire)
	}
}

// 本协议要求首条消息是 user。历史被裁剪成 assistant 起头时
// 必须补一条占位消息，否则整个请求被拒。
func TestEncodeInsertsLeadingUserMessage(t *testing.T) {
	wire, err := EncodeRequest(&ir.Request{
		Model: "claude-opus-5",
		Messages: []ir.Message{
			{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: "picking up"}}},
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "go on"}}},
		},
	})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	var got wireRequest
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("want a placeholder plus the two originals, got %d", len(got.Messages))
	}
	if got.Messages[0].Role != "user" {
		t.Fatalf("first message must be user, got %q", got.Messages[0].Role)
	}
	if !strings.Contains(string(got.Messages[0].Content), leadingUserPlaceholder) {
		t.Fatalf("placeholder text missing: %s", got.Messages[0].Content)
	}
	if got.Messages[1].Role != "assistant" || !strings.Contains(string(got.Messages[1].Content), "picking up") {
		t.Fatalf("original assistant message must survive: %+v", got.Messages[1])
	}
}

// 已经是 user 起头时不能插入：无条件插入会改变前缀，打掉上游的 prompt cache。
func TestEncodeLeavesLeadingUserAlone(t *testing.T) {
	wire, err := EncodeRequest(&ir.Request{
		Model: "claude-opus-5",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
		},
	})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	var got wireRequest
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("want the single original message, got %d", len(got.Messages))
	}
}

func TestDecodeRequestRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"malformed":     `{`,
		"no model":      `{"messages":[]}`,
		"bad content":   `{"model":"m","messages":[{"role":"user","content":42}]}`,
		"unknown block": `{"model":"m","messages":[{"role":"user","content":[{"type":"video"}]}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeRequest([]byte(body)); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

func TestRegisteredUnderProtocolName(t *testing.T) {
	in, ok := codec.Inbound(codec.ProtocolAnthropic)
	if !ok || in.Name() != Name {
		t.Fatalf("inbound not registered: %v %v", in, ok)
	}
	out, ok := codec.Outbound(codec.ProtocolAnthropic)
	if !ok || out.Name() != Name {
		t.Fatalf("outbound not registered: %v %v", out, ok)
	}
	url, _ := out.Endpoint("https://api.example.test/coding/", "claude-opus-5", true)
	if url != "https://api.example.test/coding/v1/messages" {
		t.Errorf("endpoint = %q", url)
	}
	// 版本头不再由 Endpoint 给出：它跟着客户端声明走，见 DeclarationHeaders。
	de, ok := out.(codec.DeclarationEncoder)
	if !ok {
		t.Fatal("outbound does not carry client declarations")
	}
	if got := de.DeclarationHeaders(codec.Declarations{})["anthropic-version"]; got != apiVersion {
		t.Errorf("default version = %q, want %q", got, apiVersion)
	}
}
