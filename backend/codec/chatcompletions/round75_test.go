package chatcompletions

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 本文件守 chat_completions 流内错误帧**保留** param（因而不报丢弃）这条与
// anthropic 的分界。
//
// 本协议错误信封有 error.param 槽位，上游给的 param 原样落进去，没有丢弃也就
// 没有说明；anthropic 因信封只有 {type,message} 才报这一维丢弃。把这条钉住，
// 是为了防止「给所有协议无差别补 param 丢弃说明」的过度修复——那会在本协议
// 制造假阳性注记（规则 a：说明当且仅当真丢弃）。

func TestStreamErrorPreservesParamWithoutNote(t *testing.T) {
	enc := inboundCodec{}.NewStreamEncoder(nil)
	frames := encodeEvent(t, enc, ir.Event{Type: ir.EvError, Err: &ir.Error{
		Kind: ir.ErrInvalidRequest, StatusCode: 400,
		Param: "max_tokens", Message: "bad field",
	}})

	var body []byte
	for _, f := range frames {
		body = append(body, f...)
	}
	if !strings.Contains(string(body), "max_tokens") {
		t.Errorf("chat 流内错误帧本应保留 param，却没出现 param 值：%s", body)
	}
	for _, s := range encoderNotes(t, enc) {
		if strings.Contains(s, "dropped error param") {
			t.Errorf("chat 保留了 param 却报了丢弃说明（假阳性）：%q", s)
		}
	}
}
