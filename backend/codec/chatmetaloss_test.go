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

// 这个文件守两条 chat 目标上的可见性：
//   - metadata 现在有槽位的两家（chat/responses）同族静默；anthropic 只有
//     user_id 一个键，措辞说清是受限而非全无槽位；gemini 全无槽位。
//   - modalities 值集外的值（如 image）被 chat 编码器滤掉——写出去是上游
//     必 400 的形状——滤了什么必须报出来。

func lossyFor(t *testing.T, name string, req *ir.Request) string {
	t.Helper()
	oc, ok := codec.Outbound(name)
	if !ok {
		t.Fatalf("outbound %q not registered", name)
	}
	return strings.Join(codec.DescribeLossy(req, name, oc.Caps()), "; ")
}

func TestClientMetadataNotesPerTarget(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 16,
		ClientMetadata: map[string]string{"trace": "abc"}}
	if got := lossyFor(t, codec.ProtocolChatCompletions, req); strings.Contains(got, "metadata") {
		t.Errorf("chat 有 metadata 槽位，不该报：%q", got)
	}
	if got := lossyFor(t, codec.ProtocolResponses, req); strings.Contains(got, "metadata") {
		t.Errorf("responses 有 metadata 槽位，不该报：%q", got)
	}
	got := lossyFor(t, codec.ProtocolAnthropic, req)
	if !strings.Contains(got, "keeps only the end-user id") {
		t.Errorf("anthropic 应走「只留 user_id」措辞：%q", got)
	}
	got = lossyFor(t, codec.ProtocolGemini, req)
	if !strings.Contains(got, "no client metadata parameter") {
		t.Errorf("gemini 应走「无槽位」措辞：%q", got)
	}
}

func TestChatModalitiesValueSetNote(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 16,
		Modalities: []string{"text", "image"}}
	got := lossyFor(t, codec.ProtocolChatCompletions, req)
	if !strings.Contains(got, "modalities image") ||
		!strings.Contains(got, "only text/audio") {
		t.Errorf("chat 目标未报值集外的 image：%q", got)
	}
	// 值集内静默：text/audio 原样送达，报了是假阳性。
	ok := &ir.Request{Model: "m", MaxTokens: 16,
		Modalities: []string{"text", "audio"}}
	if got := lossyFor(t, codec.ProtocolChatCompletions, ok); strings.Contains(got, "modalities") {
		t.Errorf("值集内误报：%q", got)
	}
	// audio 配置在值集内时也不触发（跨族那条 modalities/audio 注记
	// 已在 chatextrasloss_test 守过，这里只验证 chat 目标不误报）。
	if got := lossyFor(t, codec.ProtocolChatCompletions,
		&ir.Request{Model: "m", MaxTokens: 16, AudioOut: &ir.AudioOut{Format: "wav"}}); strings.Contains(got, "modalities") {
		t.Errorf("chat 目标 audio 误报：%q", got)
	}
}
