package codec

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 只带远程 URL、没有内联字节的**非图片**媒体（document/file/audio，轮次12 F1）：
// 能否投递取决于目标协议能不能凭 URL 引用非图片媒体（caps.NonImageMediaURL）。
// anthropic 的 document source.type=url、gemini 的 FileData.FileURI 逐字带回 URL，
// 不算损耗；chat_completions 的 file part、responses 的 input_file 承载非图片媒体
// 只认内联 base64（或 file_id 引用，见 nativefileref_test.go），拿到「只有 URL」的
// 附件降级成「附件已省略」文本占位——URL 没送到模型面前，模型读不到那个文件，必须报。
// 图片不在此列：四家都有图片 URL 槽位，只带 URL 的图片照常投递，不得误报。
// 内联 Data 的非图片媒体也不在此列：四家都能按 base64 投递。此前判据只看
// !AcceptsMedia(SniffMediaType)，而 SniffMediaType 能从 MediaType/Name/URL 认出真类型，
// 白名单内的 URL-only 附件因此一条注记都不报——这是第二种降级，此前无人报。

func urlDocReq() *ir.Request {
	return &ir.Request{Messages: msg(ir.Block{
		Type:  ir.BlockDocument,
		Media: &ir.Media{MediaType: "application/pdf", URL: "https://example.com/f.pdf"},
	})}
}

func urlMediaCaps(canURL bool) Capabilities {
	c := fullCaps()
	c.NonImageMediaURL = canURL
	return c
}

func TestDescribeLossyURLOnlyDocumentNotedWhenNoURLDelivery(t *testing.T) {
	got := strings.Join(DescribeLossy(urlDocReq(), "self", urlMediaCaps(false)), "\n")
	if !strings.Contains(got, "only a remote URL") {
		t.Errorf("URL-only 的非图片文档在表达不了 URL 引用的目标应报降级为文本占位:\n%s", got)
	}
}

func TestDescribeLossyURLOnlyDocumentSilentWhenURLDelivery(t *testing.T) {
	got := strings.Join(DescribeLossy(urlDocReq(), "self", urlMediaCaps(true)), "\n")
	if strings.Contains(got, "only a remote URL") {
		t.Errorf("URL-only 的非图片文档在能凭 URL 投递的目标被误报为降级:\n%s", got)
	}
}

// 内联 base64 的非图片媒体四家都投得了，不得触发「URL 无从投递」这条注记。
func TestDescribeLossyInlineDataDocumentSilentRegardless(t *testing.T) {
	req := &ir.Request{Messages: msg(ir.Block{
		Type:  ir.BlockDocument,
		Media: &ir.Media{MediaType: "application/pdf", Data: "aGk="},
	})}
	for _, canURL := range []bool{false, true} {
		got := strings.Join(DescribeLossy(req, "self", urlMediaCaps(canURL)), "\n")
		if strings.Contains(got, "only a remote URL") {
			t.Errorf("内联 base64 文档（NonImageMediaURL=%v）被误报为 URL 降级:\n%s", canURL, got)
		}
	}
}

// 图片另有 URL 槽位（四家都能按 URL 投图片），只带 URL 的图片不在此列。
func TestDescribeLossyURLOnlyImageSilentRegardless(t *testing.T) {
	req := &ir.Request{Messages: msg(ir.Block{
		Type:  ir.BlockImage,
		Media: &ir.Media{MediaType: "image/png", URL: "https://example.com/p.png"},
	})}
	for _, canURL := range []bool{false, true} {
		got := strings.Join(DescribeLossy(req, "self", urlMediaCaps(canURL)), "\n")
		if strings.Contains(got, "only a remote URL") {
			t.Errorf("URL-only 的图片（NonImageMediaURL=%v）被误报为非图片 URL 降级:\n%s", canURL, got)
		}
	}
}

func TestNonImageMediaURLCapabilityMatchesEncoders(t *testing.T) {
	// 事实出处对齐：只有 anthropic（document source.type=url）与 gemini
	// （FileData.FileURI）的出站编码器能凭 URL 投递非图片媒体。chat_completions
	// 的 file part、responses 的 input_file 承载非图片媒体只认内联 data 或 file_id。
	want := map[string]bool{
		ProtocolAnthropic:       true,
		ProtocolChatCompletions: false,
		ProtocolResponses:       false,
		ProtocolGemini:          true,
	}
	for name, expect := range want {
		oc, ok := Outbound(name)
		if !ok {
			t.Fatalf("Outbound(%s) 不可用", name)
		}
		if got := oc.Caps().NonImageMediaURL; got != expect {
			t.Errorf("%s NonImageMediaURL=%v，应为 %v", name, got, expect)
		}
	}
}
