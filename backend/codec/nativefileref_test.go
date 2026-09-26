package codec

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 非图片媒体的 file_id 引用（轮次9 F1）：base64 与 URL 两载体全空、只剩文件
// 引用时，能否投递取决于目标协议有没有原生文件引用槽位（caps.NativeFileRef）。
// chat_completions 的 file part、responses 的 input_file 逐字带回，不算损耗，
// 不得报「无可投递载荷」；anthropic 整块跳过、gemini 降级为文本，仍要报。
// 三载体（含 file_id）全空时所有目标都无从编起，一律报。此前判据只看
// !HasPayload()（FileID 刻意不算载荷），对 chat/responses 误报成「没当媒体发」。

func docRefReq(fileID string) *ir.Request {
	return &ir.Request{Messages: msg(ir.Block{
		Type: ir.BlockDocument, Media: &ir.Media{FileID: fileID},
	})}
}

func fileRefCaps(native bool) Capabilities {
	c := fullCaps()
	c.NativeFileRef = native
	return c
}

func TestDescribeLossyFileRefDocumentSilentWhenNative(t *testing.T) {
	got := strings.Join(DescribeLossy(docRefReq("file_abc"), "self", fileRefCaps(true)), "\n")
	if strings.Contains(got, "it is not sent as media") {
		t.Errorf("带 file_id 的文档在原生收引用的目标被误报为无可投递载荷:\n%s", got)
	}
}

func TestDescribeLossyFileRefDocumentNotedWhenNotNative(t *testing.T) {
	got := strings.Join(DescribeLossy(docRefReq("file_abc"), "self", fileRefCaps(false)), "\n")
	if !strings.Contains(got, "it is not sent as media") {
		t.Errorf("带 file_id 的文档在表达不了引用的目标应报无可投递载荷:\n%s", got)
	}
}

func TestDescribeLossyTrulyEmptyDocumentNotedRegardless(t *testing.T) {
	// file_id 也空：三载体全空，原生收引用的目标也无从编起，仍要报。
	got := strings.Join(DescribeLossy(docRefReq(""), "self", fileRefCaps(true)), "\n")
	if !strings.Contains(got, "it is not sent as media") {
		t.Errorf("三载体全空的文档应报无可投递载荷:\n%s", got)
	}
}

func TestNativeFileRefCapabilityMatchesEncoders(t *testing.T) {
	// 事实出处对齐：只有 chat_completions 与 responses 的出站编码器有 FileID
	// 分支（file / input_file part），故只有这两族 NativeFileRef 为真。
	// anthropic 整块跳过、gemini 降级为文本，都表达不了纯文件引用。
	want := map[string]bool{
		ProtocolAnthropic:       false,
		ProtocolChatCompletions: true,
		ProtocolResponses:       true,
		ProtocolGemini:          false,
	}
	for name, expect := range want {
		oc, ok := Outbound(name)
		if !ok {
			t.Fatalf("Outbound(%s) 不可用", name)
		}
		if got := oc.Caps().NativeFileRef; got != expect {
			t.Errorf("%s NativeFileRef=%v，应为 %v", name, got, expect)
		}
	}
}
