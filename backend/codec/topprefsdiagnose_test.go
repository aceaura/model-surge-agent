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

// anthropic 顶层 cache_control 糖与 inference_geo 的跨族诊断：顶层糖与块级
// 断点同维度（无断点参数的协议报丢弃）；inference_geo 是独立的地理偏好
// 维度，外族全无对应，值不回显。
//
// 对应旧仓 #30（41a1688）。

func topPrefsNotes(t *testing.T, req *ir.Request, name string) string {
	t.Helper()
	oc, ok := codec.Outbound(name)
	if !ok {
		t.Fatalf("outbound %q not registered", name)
	}
	return strings.Join(codec.DescribeLossy(req, name, oc.Caps()), "; ")
}

func TestDiagnoseTopCacheCtlDroppedOffAnthropic(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10, TopCacheCtl: "ephemeral", TopCacheTTL: "1h"}
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolGemini} {
		got := topPrefsNotes(t, req, name)
		if !strings.Contains(got, "dropped cache_control") {
			t.Errorf("%s 顶层糖应报缓存断点丢失：%q", name, got)
		}
	}
	if got := topPrefsNotes(t, req, codec.ProtocolAnthropic); got != "" {
		t.Errorf("anthropic 自家接得住，误报：%q", got)
	}
}

// 顶层糖与块级断点并存时同维度去重，只报一条 cache_control。
func TestDiagnoseTopCacheCtlMergesWithBlockMarks(t *testing.T) {
	req := &ir.Request{
		Model: "m", MaxTokens: 10, TopCacheCtl: "ephemeral",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockText, Text: "hi", CacheCtl: "ephemeral"},
		}}},
	}
	got := topPrefsNotes(t, req, codec.ProtocolChatCompletions)
	if strings.Count(got, "cache_control") != 1 {
		t.Errorf("块级+顶层糖应合并成一条 cache_control 说明：%q", got)
	}
}

func TestDiagnoseInferenceGeoDroppedOffAnthropic(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10, InferenceGeo: "us"}
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolGemini} {
		got := topPrefsNotes(t, req, name)
		if !strings.Contains(got, "dropped inference_geo") {
			t.Errorf("%s 应报 inference_geo 丢失：%q", name, got)
		}
		// 区域偏好值不回显（客户端自选值不入诊断）。
		if strings.Contains(got, `"us"`) || strings.Contains(got, " us") {
			t.Errorf("%s 不应回显 geo 值：%q", name, got)
		}
	}
	if got := topPrefsNotes(t, req, codec.ProtocolAnthropic); got != "" {
		t.Errorf("anthropic 自家接得住，误报：%q", got)
	}
}

func TestDiagnoseTopPrefsSilentWhenAbsent(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10}
	for _, name := range codec.OutboundNames() {
		if got := topPrefsNotes(t, req, name); got != "" {
			t.Errorf("%s 空请求误报：%q", name, got)
		}
	}
}
