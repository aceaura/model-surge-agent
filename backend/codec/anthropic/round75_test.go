package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 本文件守「anthropic 流内错误帧丢弃 param 维度」这条说明，与非流式同口径（规则 b）。
//
// anthropic 错误信封只有 {type,message} 两个位，上游给的 param 无处安放：非流式
// 经 RenderErrorLossy 报出这一维丢弃，流式此前却静默——同一件事走流式还是非流式
// 结论不一。本轮把流式 EvError 也补上同一措辞（共用 errorParamDropNote），并由
// writeStreamErrorEnd 在错误收尾时取走 Notes() 落进流水（另一半前提，见 pipeline
// 侧 round75bridge_internal_test.go）。

func paramErr(param string) *ir.Error {
	return &ir.Error{
		Kind: ir.ErrInvalidRequest, StatusCode: 400,
		Param: param, Message: "bad field",
	}
}

// paramNote 取出讲错误 param 丢弃那一条（措辞含 "dropped error param"）。
func paramNote(notes []string) (string, int) {
	found, n := "", 0
	for _, s := range notes {
		if strings.Contains(s, "dropped error param") {
			found, n = s, n+1
		}
	}
	return found, n
}

// 流式 EvError 带 param → Notes() 恰好一条 param 丢弃说明，且与非流式逐字一致。
//
// 逐字一致是规则 b 的硬要求：两条路径渲染的是同一个 {type,message} 信封、丢的是
// 同一维，措辞漂移会让运维按说明检索时以为是两种故障。
func TestStreamErrorReportsDroppedParam(t *testing.T) {
	enc := inboundCodec{}.NewStreamEncoder(nil)
	encodeEvent(t, enc, ir.Event{Type: ir.EvError, Err: paramErr("max_tokens")})
	streamNote, n := paramNote(encoderNotes(t, enc))
	if n != 1 {
		t.Fatalf("流式 notes = %#v, want 恰好一条 param 丢弃说明", encoderNotes(t, enc))
	}

	_, _, nonStreamNotes := RenderErrorLossy(paramErr("max_tokens"))
	nonStreamNote, m := paramNote(nonStreamNotes)
	if m != 1 {
		t.Fatalf("非流式 notes = %#v, want 恰好一条 param 丢弃说明", nonStreamNotes)
	}
	if streamNote != nonStreamNote {
		t.Errorf("流式说明 %q 与非流式 %q 不一致——同一件事两种措辞会被当成两种故障",
			streamNote, nonStreamNote)
	}
}

// 上游没给 param 时不出说明：普通错误不该每次都报一条噪音，真正的丢弃会被淹掉。
func TestStreamErrorSilentWithoutParam(t *testing.T) {
	enc := inboundCodec{}.NewStreamEncoder(nil)
	encodeEvent(t, enc, ir.Event{Type: ir.EvError, Err: paramErr("")})
	if _, n := paramNote(encoderNotes(t, enc)); n != 0 {
		t.Errorf("notes = %#v, want 无 param 丢弃说明", encoderNotes(t, enc))
	}
}

// 错误信封本体确实不含 param：印证「这一维真被丢了」，说明不是假阳性。
func TestStreamErrorEnvelopeOmitsParam(t *testing.T) {
	enc := inboundCodec{}.NewStreamEncoder(nil)
	frames := encodeEvent(t, enc, ir.Event{Type: ir.EvError, Err: paramErr("max_tokens")})
	var body []byte
	for _, f := range frames {
		body = append(body, f...)
	}
	if strings.Contains(string(body), "max_tokens") {
		t.Errorf("anthropic 错误信封本应无 param 槽位，却在帧里出现了 param 值：%s", body)
	}
}
