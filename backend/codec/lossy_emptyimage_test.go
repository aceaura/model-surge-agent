package codec

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 空壳图片的有损注记纠正（轮次5 F2）。三载体全空的裸图此前因 AcceptsMedia("")
// ==false 在 describeImageLossy 提前 return，最终被 describeBlocksLossy 误报成
// 「类型不支持，降级为文本」——可它根本没有载荷可降级，出站编码器是整块跳过的
// （见 anthropic 的 mediaEmptyShell）。纠正后应报「无可投递载荷」，且不再叠那条
// 自相矛盾的「降级为文本」。

// 空壳图片（裸图 / 有类型但无字节）报「无可投递载荷」，不报「降级为文本」。
func TestDescribeLossyEmptyImageReportsNoPayload(t *testing.T) {
	cases := []struct {
		name  string
		media *ir.Media
	}{
		{"bare empty image", &ir.Media{}},
		{"typed but payload-less image", &ir.Media{MediaType: "image/png"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := ir.Request{Messages: msg(ir.Block{Type: ir.BlockImage, Media: tc.media})}
			got := strings.Join(DescribeLossy(&req, "self", fullCaps()), "\n")
			if !strings.Contains(got, "carries no payload") {
				t.Errorf("expected a no-payload note, got:\n%s", got)
			}
			if strings.Contains(got, "downgraded to text") {
				t.Errorf("an empty image is skipped, not downgraded; got:\n%s", got)
			}
			// 措辞与处置无关（轮次6 F3）：空媒体的处置随目标协议而异（anthropic
			// 整块跳过、chat/gemini 降级为文本占位），统一说「不会作为媒体发出去」
			// 对各协议都准确；此前图片那条写「会被上游拒收」，对降级为文本的
			// chat/gemini 并不成立——它压根没被当媒体发出去，谈不上拒收。
			if !strings.Contains(got, "it is not sent as media") {
				t.Errorf("expected the disposition-agnostic wording, got:\n%s", got)
			}
			if strings.Contains(got, "rejected upstream") {
				t.Errorf("wording must not claim upstream rejection; got:\n%s", got)
			}
		})
	}
}

// 反向护栏：有载荷但类型不被接受的图片仍然「降级为文本」。纠正空壳误报不得顺手
// 关掉真正的降级注记，也不得给它叠上「无可投递载荷」。
func TestDescribeLossyPayloadImageBadTypeStillDowngrades(t *testing.T) {
	req := ir.Request{Messages: msg(ir.Block{Type: ir.BlockImage,
		Media: &ir.Media{MediaType: "image/gif", Data: "x"}})}
	got := strings.Join(DescribeLossy(&req, "self", fullCaps()), "\n")
	if !strings.Contains(got, "downgraded to text") {
		t.Errorf("a payload-bearing image of an unsupported type should downgrade; got:\n%s", got)
	}
	if strings.Contains(got, "carries no payload") {
		t.Errorf("this image has a payload; the no-payload note must not fire; got:\n%s", got)
	}
}

// 反向护栏：nil 载荷的图片沿用旧行为——chat 编码器会把无 Media 的图片降级成文本
// part，那条注记是准确的，F2 的 HasPayload 判据不得把它一并静默掉。
func TestDescribeLossyNilMediaImageStillDowngrades(t *testing.T) {
	req := ir.Request{Messages: msg(ir.Block{Type: ir.BlockImage})}
	got := strings.Join(DescribeLossy(&req, "self", fullCaps()), "\n")
	if !strings.Contains(got, "downgraded to text") {
		t.Errorf("a nil-media image should still be reported as downgraded; got:\n%s", got)
	}
}
