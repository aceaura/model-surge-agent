package pipeline

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 本文件守 writeStreamErrorEnd 在流内错误收尾时收集编码器累计的有损说明。
//
// 此前流内错误收尾从不取 encoder.Notes()，于是「流已交付部分内容（其间已有丢弃）
// 后以错误终止」时，全程计到的丢弃一律不落流水——与非流式 renderErrorWithLossy
// （记 param 丢弃）及流式成功收尾 writeSuccess（记 Notes）两相失衡（规则 b）。
// 这里用 anthropic 入站编码器复现：错误信封丢 param 这一维必须在错误收尾后出现
// 在 rec 的有损列里。codec 侧的同损同措辞由 anthropic/round75_test.go 守。

func TestWriteStreamErrorEndCollectsNotes(t *testing.T) {
	in, ok := codec.Inbound(codec.ProtocolAnthropic)
	if !ok {
		t.Fatal("anthropic inbound codec not registered")
	}
	enc := in.NewStreamEncoder(nil)
	rec := &Record{}
	err := &ir.Error{
		Kind: ir.ErrInvalidRequest, StatusCode: 400,
		Param: "max_tokens", Message: "bad field",
	}

	// capt 传 nil：capture.Session.Add 对 nil 接收者安全，本用例只关心 rec。
	writeStreamErrorEnd(httptest.NewRecorder(), enc, err, nil, rec)

	joined := strings.Join(rec.mergedLossy(), "\n")
	if !strings.Contains(joined, "dropped error param max_tokens") {
		t.Errorf("流内错误收尾没把 param 丢弃说明落进流水：lossy = %#v", rec.mergedLossy())
	}
}

// 没有 param 时错误收尾不该凭空多出说明：守「收集 Notes()」没退化成「无脑塞噪音」。
func TestWriteStreamErrorEndNoNoteWithoutParam(t *testing.T) {
	in, ok := codec.Inbound(codec.ProtocolAnthropic)
	if !ok {
		t.Fatal("anthropic inbound codec not registered")
	}
	enc := in.NewStreamEncoder(nil)
	rec := &Record{}
	err := &ir.Error{Kind: ir.ErrRateLimit, StatusCode: 429, Message: "slow down"}

	writeStreamErrorEnd(httptest.NewRecorder(), enc, err, nil, rec)

	for _, s := range rec.mergedLossy() {
		if strings.Contains(s, "dropped error param") {
			t.Errorf("无 param 却报了 param 丢弃说明（假阳性）：%q", s)
		}
	}
}
