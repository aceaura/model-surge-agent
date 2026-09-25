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

// 拒绝正文的跨族损耗（移植旧仓 #11）。OpenAI 两系有专属槽位（chat 的
// message.refusal、responses 的 refusal part），同族往返不降级；anthropic
// 与 gemini 没有槽位，正文降级为普通文本而不是丢弃——拒绝正文是模型真正
// 说出的话，丢了客户端只剩空消息配一个拒绝标记。降级不加标注前缀：正文
// 会成为模型后续轮次读到的自己说过的话。请求侧 DescribeLossy 报 merged。

func refusalReq() *ir.Request {
	return &ir.Request{Model: "m", MaxTokens: 16, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "do it"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{
			{Type: ir.BlockRefusal, Text: "I cannot help with that."}}},
	}}
}

// 请求侧诊断：两系静默（原样回槽位），anthropic/gemini 报 merged 计数。
func TestDiagnoseRefusalMerged(t *testing.T) {
	for _, name := range []string{codec.ProtocolAnthropic, codec.ProtocolGemini} {
		oc, _ := codec.Outbound(name)
		got := strings.Join(codec.DescribeLossy(refusalReq(), name, oc.Caps()), "; ")
		if !strings.Contains(got, "merged 1 refusal(s) into plain text") {
			t.Errorf("%s 应报拒绝并入文本：%q", name, got)
		}
		if strings.Contains(got, "I cannot help") {
			t.Errorf("%s 注记泄漏拒绝正文：%q", name, got)
		}
	}
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses} {
		oc, _ := codec.Outbound(name)
		if notes := codec.DescribeLossy(refusalReq(), name, oc.Caps()); len(notes) != 0 {
			t.Errorf("%s 自家接得住，误报：%v", name, notes)
		}
	}
	// 缺席全静默。
	empty := &ir.Request{Model: "m", MaxTokens: 16}
	for _, name := range codec.OutboundNames() {
		oc, _ := codec.Outbound(name)
		if notes := codec.DescribeLossy(empty, name, oc.Caps()); len(notes) != 0 {
			t.Errorf("%s 无拒绝误报：%v", name, notes)
		}
	}
}

// anthropic 出站：拒绝正文降级为文本块，不加标注前缀，也不整块丢弃。
func TestAnthropicRefusalDegradesToText(t *testing.T) {
	oc, _ := codec.Outbound(codec.ProtocolAnthropic)
	body, err := oc.EncodeRequest(refusalReq())
	if err != nil {
		t.Fatalf("anthropic EncodeRequest: %v", err)
	}
	if !strings.Contains(string(body), `"type":"text","text":"I cannot help with that."`) {
		t.Fatalf("拒绝正文没有降级为文本块: %s", body)
	}
}

// gemini 出站：同样降级为文本 part。
func TestGeminiRefusalDegradesToText(t *testing.T) {
	oc, _ := codec.Outbound(codec.ProtocolGemini)
	body, err := oc.EncodeRequest(refusalReq())
	if err != nil {
		t.Fatalf("gemini EncodeRequest: %v", err)
	}
	if !strings.Contains(string(body), `"text":"I cannot help with that."`) {
		t.Fatalf("拒绝正文没有降级为文本 part: %s", body)
	}
}

// 响应侧：外族客户端编码器把拒绝块编成文本（可见内容），不得整块丢弃
// 或报错；两系客户端回专属槽位。
func TestRefusalResponseCrossFamily(t *testing.T) {
	resp := &ir.Response{ID: "m", Model: "m", StopReason: ir.StopContentFilter,
		Content: []ir.Block{{Type: ir.BlockRefusal, Text: "secret refusal text"}}}
	for _, name := range []string{codec.ProtocolAnthropic, codec.ProtocolChatCompletions, codec.ProtocolResponses} {
		ic, _ := codec.Inbound(name)
		le := ic.(codec.LossyResponseEncoder)
		body, _, err := le.EncodeResponseLossy(resp)
		if err != nil {
			t.Fatalf("%s EncodeResponseLossy: %v", name, err)
		}
		if !strings.Contains(string(body), "secret refusal text") {
			t.Errorf("%s 拒绝正文丢失: %s", name, body)
		}
		switch name {
		case codec.ProtocolAnthropic:
			// 降级为文本块，不加标注前缀。
			if !strings.Contains(string(body), `"type":"text","text":"secret refusal text"`) {
				t.Errorf("anthropic 拒绝没有降级为文本块: %s", body)
			}
		case codec.ProtocolChatCompletions:
			if !strings.Contains(string(body), `"refusal":"secret refusal text"`) {
				t.Errorf("chat 拒绝没回 refusal 槽位: %s", body)
			}
		case codec.ProtocolResponses:
			if !strings.Contains(string(body), `"type":"refusal"`) {
				t.Errorf("responses 拒绝没回 refusal part: %s", body)
			}
		}
	}
}
