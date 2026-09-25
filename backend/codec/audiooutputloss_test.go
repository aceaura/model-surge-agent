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

// 音频维度的跨族损耗（移植旧仓 #35）。完整音频输出（id/data/expires_at/
// transcript）只有 chat 非流式一个槽位；多轮音频引用（{audio:{id}}）只有
// chat 请求侧一个槽位。其余协议丢弃必须经三通道照实报出：非流式
// EncodeResponseLossy、流式 Notes()、请求侧 DescribeLossy。注记不得泄漏
// 音频内容本身（data 是用户语音的 base64，transcript 是转写文本）。

var rSecretAudio = &ir.AudioOutput{ID: "audio_secret_id", Data: "audio_secret_data",
	ExpiresAt: 1893456000, Transcript: "secret transcript"}

const audioDropNote = "dropped model audio output"
const audioRefNote = "assistant audio reference(s)"

// inbound 非流式：chat 自家接得住，外族报丢弃且不泄漏。
func TestAudioOutputResponseNotes(t *testing.T) {
	resp := &ir.Response{ID: "m", Model: "m",
		Content: []ir.Block{{Type: ir.BlockText, Text: "caption"}},
		Audio:   rSecretAudio}
	for _, name := range []string{codec.ProtocolAnthropic, codec.ProtocolResponses} {
		notes := responseLossyNotes(t, name, resp)
		joined := strings.Join(notes, "; ")
		if !strings.Contains(joined, audioDropNote) {
			t.Errorf("%s 应报音频丢弃：%v", name, notes)
		}
		assertNoAudioSecret(t, name+" notes", joined)
	}
	if notes := responseLossyNotes(t, codec.ProtocolChatCompletions, resp); len(notes) != 0 {
		t.Errorf("chat 自家接得住，误报：%v", notes)
	}
	// 无音频全静默。
	resp.Audio = nil
	for _, name := range []string{codec.ProtocolAnthropic, codec.ProtocolResponses, codec.ProtocolChatCompletions} {
		if notes := responseLossyNotes(t, name, resp); len(notes) != 0 {
			t.Errorf("%s 无音频误报：%v", name, notes)
		}
	}
}

// 外族非流式响应体不得带出音频内容，但正文照常。
func TestAudioOutputForeignNonStreamDoesNotLeak(t *testing.T) {
	resp := &ir.Response{ID: "m", Model: "m",
		Content: []ir.Block{{Type: ir.BlockText, Text: "caption"}},
		Audio:   rSecretAudio}
	for _, name := range []string{codec.ProtocolAnthropic, codec.ProtocolResponses} {
		ic, ok := codec.Inbound(name)
		if !ok {
			t.Fatalf("inbound %q not registered", name)
		}
		le := ic.(codec.LossyResponseEncoder)
		body, _, err := le.EncodeResponseLossy(resp)
		if err != nil {
			t.Fatalf("%s EncodeResponseLossy: %v", name, err)
		}
		assertNoAudioSecret(t, name+" body", string(body))
		if !strings.Contains(string(body), "caption") {
			t.Errorf("%s 正文丢失: %s", name, body)
		}
	}
}

// 流式：所有入站协议（含 chat 自己——流式 chat delta 没有音频槽位）都丢弃
// 并报注记；重复携带合并为一条；正文存活。完整音频只随 ResponseEvents 投影
// 的 EvMessageStart 到达，真流式上游给不出这一维。
func TestAudioOutputStreamEncodeDropped(t *testing.T) {
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolAnthropic, codec.ProtocolResponses} {
		ic, _ := codec.Inbound(name)
		e := ic.NewStreamEncoder(nil)
		if _, err := e.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "m", Model: "m", Audio: rSecretAudio}); err != nil {
			t.Fatalf("%s Encode start: %v", name, err)
		}
		e.Encode(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}})
		e.Encode(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "caption"})
		e.Encode(ir.Event{Type: ir.EvBlockStop, Index: 0})
		e.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn})
		e.Encode(ir.Event{Type: ir.EvMessageStop})
		got := streamNotes(e)
		if !strings.Contains(got, audioDropNote) {
			t.Errorf("%s 首帧丢音频应报：%q", name, got)
		}
		assertNoAudioSecret(t, name+" stream notes", got)

		// 重复携带合并为一条（Notes 出口去重）。
		e3 := ic.NewStreamEncoder(nil)
		e3.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "m", Model: "m", Audio: rSecretAudio})
		e3.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Audio: rSecretAudio})
		if n := strings.Count(streamNotes(e3), audioDropNote); n != 1 {
			t.Errorf("%s 重复携带应合并为一条，实得 %d：%q", name, n, streamNotes(e3))
		}
	}
}

// 请求侧诊断：chat 自家有引用槽位，静默；外族报引用丢弃且不泄漏 id。
func TestDiagnoseAudioRefDropped(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10, Messages: []ir.Message{
		{Role: ir.RoleAssistant, AudioID: "audio_secret_1"},
	}}
	for _, name := range []string{codec.ProtocolAnthropic, codec.ProtocolResponses, codec.ProtocolGemini} {
		oc, _ := codec.Outbound(name)
		got := strings.Join(codec.DescribeLossy(req, name, oc.Caps()), "; ")
		if !strings.Contains(got, "dropped 1 "+audioRefNote) {
			t.Errorf("%s 应报音频引用丢失：%q", name, got)
		}
		if strings.Contains(got, "audio_secret_1") {
			t.Errorf("%s 注记泄漏引用 id：%q", name, got)
		}
	}
	oc, _ := codec.Outbound(codec.ProtocolChatCompletions)
	if notes := codec.DescribeLossy(req, codec.ProtocolChatCompletions, oc.Caps()); len(notes) != 0 {
		t.Errorf("chat 自家接得住，误报：%v", notes)
	}
}

// 计数只认 assistant 引用；user 消息上的 AudioID（非法来路）不计。
func TestDiagnoseAudioRefCountAndRole(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10, Messages: []ir.Message{
		{Role: ir.RoleAssistant, AudioID: "a1"},
		{Role: ir.RoleUser, AudioID: "must_not_count"},
		{Role: ir.RoleAssistant, AudioID: "a2"},
	}}
	oc, _ := codec.Outbound(codec.ProtocolGemini)
	got := strings.Join(codec.DescribeLossy(req, codec.ProtocolGemini, oc.Caps()), "; ")
	if !strings.Contains(got, "dropped 2 "+audioRefNote) {
		t.Errorf("应计 2 条 assistant 引用：%q", got)
	}
	// 缺席全静默。
	req.Messages = nil
	for _, name := range codec.OutboundNames() {
		oc, _ := codec.Outbound(name)
		if notes := codec.DescribeLossy(req, name, oc.Caps()); len(notes) != 0 {
			t.Errorf("%s 无音频引用误报：%v", name, notes)
		}
	}
}

// AudioID-only 的 assistant 历史投到 anthropic：引用无槽位（DescribeLossy
// 已报），但消息本体不得以 content:[] 形态出站——空数组会被上游按校验拒
// 整轮，落约定占位。
func TestAnthropicAudioOnlyMessageGetsPlaceholder(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10, Messages: []ir.Message{
		{Role: ir.RoleAssistant, AudioID: "audio_1"},
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "next"}}},
	}}
	oc, _ := codec.Outbound(codec.ProtocolAnthropic)
	body, err := oc.EncodeRequest(req)
	if err != nil {
		t.Fatalf("anthropic EncodeRequest: %v", err)
	}
	if strings.Contains(string(body), `"content":[]`) {
		t.Fatalf("AudioID-only 消息以空 content 出站: %s", body)
	}
	if !strings.Contains(string(body), codec.ConversationPlaceholder) {
		t.Fatalf("缺占位: %s", body)
	}
}

// assertNoAudioSecret 断言文本不带音频内容三敏感值（id 除外——请求侧注记
// 单独断言 id；此处覆盖响应侧全部三值）。
func assertNoAudioSecret(t *testing.T, where, s string) {
	t.Helper()
	for _, secret := range []string{"audio_secret_id", "audio_secret_data", "secret transcript", "UklGRg"} {
		if strings.Contains(s, secret) {
			t.Errorf("%s 泄漏音频内容 %q：%s", where, secret, s)
		}
	}
}
