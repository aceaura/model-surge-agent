package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次45：transformations.oversized_image 是 anthropic 图片块专属的渲染指令（官方
// image_block_param.transformations.oversized_image，值 "downsize"|"error"：图片超大
// 时上游是缩小还是报错）。此前 wireBlock 未建模该键，逐字段重建的 image 块被
// json.Unmarshal 静默吞掉——同族多轮历史里客户端回传上一轮带指令的图片块时往返不再
// 逐字，客户端对超大图片的处置意图丢失。这组测试钉住解码入 IR、同族编码回吐、缺席
// 不写键、不经解码器的 IR 直构路径，以及熬过 Clone。

const imgBody = `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"},` +
	`"transformations":{"oversized_image":"downsize"}}`

func TestOversizedImageDecodedIntoIR(t *testing.T) {
	reqBody := `{"model":"claude-opus-5","messages":[{"role":"user","content":[` + imgBody + `]}]}`
	req, err := DecodeRequest([]byte(reqBody))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	m := req.Messages[0].Content[0].Media
	if m == nil {
		t.Fatalf("no Media payload: %#v", req.Messages[0].Content[0])
	}
	if m.OversizedImage != "downsize" {
		t.Fatalf("oversized_image = %q，没读进 IR", m.OversizedImage)
	}
}

func TestOversizedImageRoundTripsVerbatim(t *testing.T) {
	reqBody := `{"model":"claude-opus-5","messages":[{"role":"user","content":[` + imgBody + `]}]}`
	req, err := DecodeRequest([]byte(reqBody))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	wire, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(wire), `"oversized_image":"downsize"`) {
		t.Fatalf("oversized_image lost on re-encode: %s", wire)
	}
}

func TestNoOversizedImageWhenAbsent(t *testing.T) {
	body := `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}`
	reqBody := `{"model":"claude-opus-5","messages":[{"role":"user","content":[` + body + `]}]}`
	req, err := DecodeRequest([]byte(reqBody))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	wire, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if strings.Contains(string(wire), "transformations") || strings.Contains(string(wire), "oversized_image") {
		t.Fatalf("absent oversized_image should not emit key: %s", wire)
	}
}

// 编码侧单测：直接从 IR 造带 OversizedImage 的图片块，验证 encodeBlock 逐字写出
// （覆盖不经解码器的构造路径）。
func TestEncodeBlockWritesOversizedImageFromIR(t *testing.T) {
	wb, ok, err := encodeBlock(ir.Block{Type: ir.BlockImage, Media: &ir.Media{
		MediaType: "image/png", Data: "AAAA", OversizedImage: "error",
	}})
	if err != nil || !ok {
		t.Fatalf("encodeBlock: ok=%v err=%v", ok, err)
	}
	b, _ := json.Marshal(wb)
	if !strings.Contains(string(b), `"oversized_image":"error"`) {
		t.Fatalf("oversized_image not written: %s", b)
	}
}

// OversizedImage 必须熬过 Clone：请求侧编码会 Clone 整份请求，字符串字段随
// cloneBlocks 的 `v := *b.Media` 值拷贝自动带上，此测钉住不漏新字段。
func TestOversizedImageSurvivesClone(t *testing.T) {
	req := &ir.Request{Model: "m", Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockImage, Media: &ir.Media{
			MediaType: "image/png", Data: "AAAA", OversizedImage: "downsize",
		}}}},
	}}
	cl := req.Clone()
	m := cl.Messages[0].Content[0].Media
	if m == nil || m.OversizedImage != "downsize" {
		t.Fatalf("oversized_image 没熬过 Clone：%#v", m)
	}
}
