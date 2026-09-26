package anthropic

import (
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 空壳媒体（有媒体块但 base64/URL/文件引用三者全空）走 encodeBlock 的整块
// 跳过路径、不是降级。请求侧同类空壳由 describeBlocksLossy 报「carries no
// payload」，响应侧此前静默——而 encode_request.go 跳过分支的注释自称「损耗
// 由有损诊断报出」，那句在响应路径上并不成立。这组测试钉住补齐后的行为，并
// 与降级注记分账（空壳注记措辞不含「from the model output」）。

const emptyMediaNote = "carried no payload"

// 非流式：无载荷的音频块被跳过，必须报出。
func TestResponseLossyReportsEmptyShellMedia(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{
		Type:  ir.BlockAudio,
		Media: &ir.Media{MediaType: "audio/wav"}, // 无 Data 无 URL
	}}}
	_, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !hasNoteSubstr(notes, emptyMediaNote) {
		t.Errorf("非流式空壳媒体没被报出：%#v", notes)
	}
}

// 非流式：只带 file_id 的图片——HasPayload() 刻意排除 FileID，本协议的图片
// 槽位也不认文件引用，故对 anthropic 就是空壳。这是最现实的可达形状
// （Responses 上游按引用返回图片）。
func TestResponseLossyReportsFileIDOnlyMediaAsEmpty(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{
		Type:  ir.BlockImage,
		Media: &ir.Media{FileID: "file_abc"},
	}}}
	_, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !hasNoteSubstr(notes, emptyMediaNote) {
		t.Errorf("file_id-only 媒体没被报为空壳：%#v", notes)
	}
}

// 有载荷的图片本协议装得下，不得误报为空壳。
func TestResponseLossyKeepsPayloadMediaNotReportedEmpty(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{
		Type:  ir.BlockImage,
		Media: &ir.Media{MediaType: "image/png", Data: "AAAA"},
	}}}
	_, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatal(err)
	}
	if hasNoteSubstr(notes, emptyMediaNote) {
		t.Errorf("有载荷图片被误报为空壳：%#v", notes)
	}
}

// 流式：同一空壳经 Notes() 报出，判据与非流式同源（mediaEmptyShell）。
func TestStreamReportsEmptyShellMedia(t *testing.T) {
	enc := inboundCodec{}.NewStreamEncoder(nil)
	encodeEvent(t, enc, ir.Event{Type: ir.EvBlockStart, Index: 0,
		Block: &ir.Block{Type: ir.BlockDocument, Media: &ir.Media{MediaType: "application/pdf"}}})

	if notes := encoderNotes(t, enc); !hasNoteSubstr(notes, emptyMediaNote) {
		t.Errorf("流式空壳媒体没被报出：%#v", notes)
	}
}

// 流式：有载荷的图片不得误报为空壳。
func TestStreamKeepsPayloadMediaNotReportedEmpty(t *testing.T) {
	enc := inboundCodec{}.NewStreamEncoder(nil)
	encodeEvent(t, enc, ir.Event{Type: ir.EvBlockStart, Index: 0,
		Block: &ir.Block{Type: ir.BlockImage, Media: &ir.Media{MediaType: "image/png", Data: "AAAA"}}})

	if notes := encoderNotes(t, enc); hasNoteSubstr(notes, emptyMediaNote) {
		t.Errorf("流式有载荷图片被误报为空壳：%#v", notes)
	}
}
