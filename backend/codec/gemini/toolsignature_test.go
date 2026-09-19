package gemini

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 本文件守「函数调用自身携带的推理签名」。判据 8–12。
//
// 本协议把 thoughtSignature 挂在 functionCall part 上，而不是 thought part 上。
// 此前 IR 的签名只有思考块那一位，于是这一处的签名在解码时就没有落点——
// 整条丢掉，既不报错也不出说明。工具回合是最需要它的场景：下一轮要带着
// 签名回去上游才认这次调用的推理过程。

// 判据 8：流式路径把 functionCall 上的签名挂到 tool_use 块上。
func TestStreamToolCallCarriesItsOwnSignature(t *testing.T) {
	raw := "data: " + `{"responseId":"r1","candidates":[{"content":{"parts":[` +
		`{"functionCall":{"name":"grep","args":{"q":"x"}},"thoughtSignature":"sig-call"}` +
		`]}}]}` + "\n\n"
	resp := aggregateStream(t, raw)

	use := onlyToolUse(t, resp)
	if use.Signature != "sig-call" {
		t.Errorf("签名 = %q，want sig-call——丢了它下一轮上游不认这次调用", use.Signature)
	}
	if use.SignatureFrom != Name {
		t.Errorf("来源 = %q，want %q；来源为空会被本协议自己的回写判成异族而剥离",
			use.SignatureFrom, Name)
	}
}

// 判据 9：非流式路径与流式同一处置。
//
// 两条路径分别实现，只测一条的话另一条的缺失要等线上才暴露。
func TestNonStreamToolCallCarriesItsOwnSignature(t *testing.T) {
	body := `{"responseId":"r1","candidates":[{"finishReason":"STOP","content":{"parts":[` +
		`{"functionCall":{"name":"grep","args":{"q":"x"}},"thoughtSignature":"sig-call"}` +
		`]}}]}`
	resp, err := DecodeResponse([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	use := onlyToolUse(t, resp)
	if use.Signature != "sig-call" {
		t.Errorf("签名 = %q，want sig-call", use.Signature)
	}
	if use.SignatureFrom != Name {
		t.Errorf("来源 = %q，want %q", use.SignatureFrom, Name)
	}
}

// 判据 10：本族签名回写到 functionCall part 自身。
func TestEncodeWritesOwnToolSignatureBackOntoThePart(t *testing.T) {
	body := encodeWithToolUse(t, &ir.ToolUse{
		ID: "call_1", Name: "grep", Input: `{"q":"x"}`,
		Signature: "sig-call", SignatureFrom: Name,
	})
	part := functionCallPart(t, body)
	if part.ThoughtSignature != "sig-call" {
		t.Errorf("thoughtSignature = %q，want sig-call——不回写等于每轮都把凭据扔掉",
			part.ThoughtSignature)
	}
}

// 判据 11：异族签名不回写，且请求侧出说明。
//
// 把别家的密文发过来上游会拒整轮，比丢掉更坏；但静默丢掉又让运维查不出
// 为什么跨协议的工具回合质量下降，所以两件事都要做。
func TestForeignToolSignatureIsStrippedWithANote(t *testing.T) {
	req := &ir.Request{Model: "gemini-3-pro", Messages: []ir.Message{{
		Role: ir.RoleAssistant, Content: []ir.Block{{
			Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
				ID: "call_1", Name: "grep", Input: `{"q":"x"}`,
				Signature: "sig-from-elsewhere", SignatureFrom: codec.ProtocolAnthropic,
			},
		}},
	}}}
	body, notes, err := outboundCodec{}.EncodeRequestLossy(req)
	if err != nil {
		t.Fatalf("EncodeRequestLossy: %v", err)
	}
	part := functionCallPart(t, body)
	if part.ThoughtSignature != "" {
		t.Errorf("异族密文被发给了上游：%q——上游会拒整轮", part.ThoughtSignature)
	}
	if !hasNoteAbout(notes, "tool call signature") {
		t.Errorf("剥离了却一声不响：%#v", notes)
	}
}

// 判据 12：上游没给签名时字段整个省略，不出说明。
//
// 空字符串写进 part 会让 wire 上多一个空字段；出说明会让每次普通工具调用
// 都报一条噪音，真正的丢弃被淹掉。
func TestAbsentToolSignatureIsOmittedSilently(t *testing.T) {
	req := &ir.Request{Model: "gemini-3-pro", Messages: []ir.Message{{
		Role: ir.RoleAssistant, Content: []ir.Block{{
			Type:    ir.BlockToolUse,
			ToolUse: &ir.ToolUse{ID: "call_1", Name: "grep", Input: `{"q":"x"}`},
		}},
	}}}
	body, notes, err := outboundCodec{}.EncodeRequestLossy(req)
	if err != nil {
		t.Fatalf("EncodeRequestLossy: %v", err)
	}
	if strings.Contains(string(body), "thoughtSignature") {
		t.Errorf("没有签名却写了字段：%s", body)
	}
	if hasNoteAbout(notes, "tool call signature") {
		t.Errorf("上游没给不是丢弃：%#v", notes)
	}
}

// 判据 13：本协议声明自己的函数调用带签名字段。
//
// 这一位决定别的协议要不要为它出剥离说明。没有它，剥离判据只能靠协议名
// 硬编码，新增协议时会漏。
func TestCapabilityDeclaresToolCallSignature(t *testing.T) {
	if !(outboundCodec{}.Caps().ToolCallSig) {
		t.Error("本协议的 functionCall 带 thoughtSignature，能力位必须为真")
	}
}

func onlyToolUse(t *testing.T, resp *ir.Response) *ir.ToolUse {
	t.Helper()
	var found *ir.ToolUse
	for _, b := range resp.Content {
		if b.Type == ir.BlockToolUse && b.ToolUse != nil {
			if found != nil {
				t.Fatalf("响应里有多个 tool_use 块：%+v", resp.Content)
			}
			found = b.ToolUse
		}
	}
	if found == nil {
		t.Fatalf("响应里没有 tool_use 块：%+v", resp.Content)
	}
	return found
}

func encodeWithToolUse(t *testing.T, use *ir.ToolUse) []byte {
	t.Helper()
	body, err := EncodeRequest(&ir.Request{
		Model: "gemini-3-pro",
		Messages: []ir.Message{{
			Role:    ir.RoleAssistant,
			Content: []ir.Block{{Type: ir.BlockToolUse, ToolUse: use}},
		}},
	})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	return body
}

func functionCallPart(t *testing.T, body []byte) wirePart {
	t.Helper()
	var w struct {
		Contents []struct {
			Parts []wirePart `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	for _, c := range w.Contents {
		for _, p := range c.Parts {
			if p.FunctionCall != nil {
				return p
			}
		}
	}
	t.Fatalf("请求体里没有 functionCall part：%s", body)
	return wirePart{}
}

func hasNoteAbout(notes []string, what string) bool {
	for _, n := range notes {
		if strings.Contains(n, what) {
			return true
		}
	}
	return false
}
