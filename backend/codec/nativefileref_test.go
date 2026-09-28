package codec

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 非图片媒体的 file_id 引用（轮次9 F1）：base64 与 URL 两载体全空、只剩文件
// 引用时，能否投递取决于目标协议有没有原生文件引用槽位（caps.NativeFileRef）。
// chat_completions 的 file part、responses 的 input_file 逐字带回，不算损耗，
// 不得报「无可投递载荷」；gemini 降级为文本，仍要报。anthropic 没有通用文件
// part（NativeFileRef 假），但有按类型定死的 document file 源
//（FileDocumentSourceParam）：文档的 file_id 引用同族可逐字投递、不报，而音频与
// 未知类型通用文件仍无从投递、照报（见下方 anthropic 专项用例）。
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
	// 事实出处对齐：NativeFileRef 指「通用文件引用槽位，document/file/audio 皆可
	// 凭 file_id 投递」。只有 chat_completions（file part）与 responses（input_file）
	// 满足，故这两族为真。anthropic 只有按类型定死的 image / document(PDF) file 源
	//（图片走 ImageFileRef，文档见 TestDescribeLossyAnthropicDocFileRefSilent），
	// 音频与通用文件无从投递 → NativeFileRef 仍为假；gemini 降级为文本，亦为假。
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

// anthropic 的 document file 源（FileDocumentSourceParam）：文档只带 file_id 时
// 同族可逐字投递，不报「无可投递载荷」。
func TestDescribeLossyAnthropicDocFileRefSilent(t *testing.T) {
	oc, ok := Outbound(ProtocolAnthropic)
	if !ok {
		t.Fatalf("Outbound(anthropic) 不可用")
	}
	got := strings.Join(DescribeLossy(docRefReq("file_abc"), ProtocolAnthropic, oc.Caps()), "\n")
	if strings.Contains(got, "it is not sent as media") {
		t.Errorf("anthropic 文档 file_id 引用应可投递、不该报无可投递载荷:\n%s", got)
	}
}

// 但音频的 file_id 引用 anthropic 没有对应源（只读图片与 PDF），仍要报——
// 防止把 ImageFileRef/document 例外误扩成「anthropic 收一切 file_id」。
func TestDescribeLossyAnthropicAudioFileRefStillNoted(t *testing.T) {
	oc, ok := Outbound(ProtocolAnthropic)
	if !ok {
		t.Fatalf("Outbound(anthropic) 不可用")
	}
	req := &ir.Request{Messages: msg(ir.Block{
		Type: ir.BlockAudio, Media: &ir.Media{FileID: "file_audio"},
	})}
	got := strings.Join(DescribeLossy(req, ProtocolAnthropic, oc.Caps()), "\n")
	if !strings.Contains(got, "it is not sent as media") {
		t.Errorf("anthropic 音频 file_id 引用无从投递，应报无可投递载荷:\n%s", got)
	}
}

// anthropic 的 image file 源（FileImageSourceParam）：图片只带 file_id 时同族可
// 逐字投递（ImageFileRef 为真），不报「图片引用无从投递」。
func TestDescribeLossyAnthropicImageFileRefSilent(t *testing.T) {
	oc, ok := Outbound(ProtocolAnthropic)
	if !ok {
		t.Fatalf("Outbound(anthropic) 不可用")
	}
	if !oc.Caps().ImageFileRef {
		t.Fatalf("anthropic ImageFileRef 应为真（官方 image_block 含 FileImageSourceParam）")
	}
	req := &ir.Request{Messages: msg(ir.Block{
		Type: ir.BlockImage, Media: &ir.Media{FileID: "file_img"},
	})}
	got := strings.Join(DescribeLossy(req, ProtocolAnthropic, oc.Caps()), "\n")
	if strings.Contains(got, "cannot point at a file-service id") {
		t.Errorf("anthropic 图片 file_id 引用应可投递、不该报图片引用丢失:\n%s", got)
	}
}
