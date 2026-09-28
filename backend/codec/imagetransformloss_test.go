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

// 轮次45：anthropic 图片块的 transformations.oversized_image（官方 image_block_param
// 专属渲染指令，值 downsize|error）是纯指令性 provenance，只有 anthropic 有槽位。
// 同族逐字往返无损；投给外族整维丢弃，此前完全静默。这组测试钉住跨族请求侧有损诊断
// 报出、anthropic 自家静默、缺席静默。

func oversizedImageReq(oversized string) *ir.Request {
	m := &ir.Media{MediaType: "image/png", Data: "AAAA"}
	if oversized != "" {
		m.OversizedImage = oversized
	}
	return &ir.Request{Model: "m", MaxTokens: 10, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockImage, Media: m}}},
	}}
}

func TestDiagnoseOversizedImageDroppedOffAnthropic(t *testing.T) {
	req := oversizedImageReq("downsize")
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolGemini} {
		oc, _ := codec.Outbound(name)
		got := strings.Join(codec.DescribeLossy(req, name, oc.Caps()), "; ")
		if !strings.Contains(got, "oversized_image directive") {
			t.Errorf("%s 应报 image oversized_image 丢失：%q", name, got)
		}
	}
	oc, _ := codec.Outbound(codec.ProtocolAnthropic)
	if notes := codec.DescribeLossy(req, codec.ProtocolAnthropic, oc.Caps()); len(notes) != 0 {
		t.Errorf("anthropic 自家接得住，误报：%v", notes)
	}
}

// 缺席不报：没有 oversized_image 的普通图片块跨族不该凭空多出该注记。
func TestNoOversizedImageNoteWhenAbsent(t *testing.T) {
	req := oversizedImageReq("")
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolGemini} {
		oc, _ := codec.Outbound(name)
		for _, n := range codec.DescribeLossy(req, name, oc.Caps()) {
			if strings.Contains(n, "oversized_image directive") {
				t.Errorf("%s 缺席 oversized_image 却报了注记：%q", name, n)
			}
		}
	}
}
