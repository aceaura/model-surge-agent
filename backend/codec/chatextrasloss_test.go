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

// chat 专属四维的跨族损耗可见性：三个外族目标都要照实报出，chat 同族
// 静默，缺席全静默。modalities 与 audio 合一则（audio 依附模态，模态丢了
// 音频配置必然随之丢），prediction 与 web_search_options 各自独立。
func TestDiagnoseChatExtrasDroppedOffChat(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10,
		Modalities:       []string{"text", "audio"},
		AudioOut:         &ir.AudioOut{Format: "wav", Voice: "alloy"},
		Prediction:       json.RawMessage(`{"type":"content","content":"x"}`),
		WebSearchOptions: json.RawMessage(`{"search_context_size":"high"}`),
	}
	for _, name := range []string{codec.ProtocolAnthropic, codec.ProtocolResponses, codec.ProtocolGemini} {
		oc, ok := codec.Outbound(name)
		if !ok {
			t.Fatalf("outbound %q not registered", name)
		}
		got := strings.Join(codec.DescribeLossy(req, name, oc.Caps()), "; ")
		for _, want := range []string{"modalities/audio", "prediction", "web_search_options"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s 应报 %s 丢失：%q", name, want, got)
			}
		}
		// 值不回显：音色名与检索档位是客户端自选值，不入诊断。
		if strings.Contains(got, "alloy") || strings.Contains(got, `"high"`) {
			t.Errorf("%s 诊断回显了客户端值：%q", name, got)
		}
	}
	// chat 同族原样往返，报了就是谎报。
	oc, _ := codec.Outbound(codec.ProtocolChatCompletions)
	if notes := codec.DescribeLossy(req, codec.ProtocolChatCompletions, oc.Caps()); len(notes) != 0 {
		t.Errorf("chat 同族误报：%v", notes)
	}
	// 只给 audio 不给 modalities 也要报（audio 依附模态，跨族一样丢）。
	audioOnly := &ir.Request{Model: "m", MaxTokens: 10,
		AudioOut: &ir.AudioOut{Format: "wav"}}
	ocA, _ := codec.Outbound(codec.ProtocolAnthropic)
	got := strings.Join(codec.DescribeLossy(audioOnly, codec.ProtocolAnthropic, ocA.Caps()), "; ")
	if !strings.Contains(got, "modalities/audio") {
		t.Errorf("只给 audio 也应报模态丢失：%q", got)
	}
	// 缺席全静默。
	bare := &ir.Request{Model: "m", MaxTokens: 10}
	for _, name := range codec.OutboundNames() {
		oc, _ := codec.Outbound(name)
		if notes := codec.DescribeLossy(bare, name, oc.Caps()); len(notes) != 0 {
			t.Errorf("%s 四维缺席误报：%v", name, notes)
		}
	}
}
