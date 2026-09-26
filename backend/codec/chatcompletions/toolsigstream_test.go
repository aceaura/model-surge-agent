package chatcompletions

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 本文件守「流式路径的工具调用推理签名剥离说明」，与非流式同口径。
//
// gemini 上游把 thoughtSignature 挂在 functionCall part 自身，解码后落进
// tool_use 块的 Signature。本协议的 tool_calls 没有承载槽位，丢弃不可避免——
// 但无声地丢会让 stream:true 的运维查不到「上游给过推理凭据」，而同一份响应
// stream:false 时 DescribeResponseToolSignatureLoss 却报了：两条路径结论不一。
// 此前流式编码器只在 EvSigDelta（思考块）上报签名，tool_use 块开启帧上的
// 签名整条蒸发且不留痕。

func toolSigBlock(sig, from string) *ir.Block {
	return &ir.Block{
		Type: ir.BlockToolUse,
		ToolUse: &ir.ToolUse{
			ID: "call_1", Name: "grep", Input: `{"q":"x"}`,
			Signature: sig, SignatureFrom: from,
		},
	}
}

func firstToolSigNote(notes []string) (string, int) {
	found, n := "", 0
	for _, s := range notes {
		if strings.Contains(s, "tool call reasoning signature") {
			found, n = s, n+1
		}
	}
	return found, n
}

func TestStreamEncoderReportsDroppedToolSignature(t *testing.T) {
	enc := inboundCodec{}.NewStreamEncoder(nil)
	encodeEvent(t, enc, ir.Event{
		Type: ir.EvBlockStart, Index: 0,
		Block: toolSigBlock("sig-call", codec.ProtocolGemini),
	})
	streamNote, n := firstToolSigNote(encoderNotes(t, enc))
	if n != 1 {
		t.Fatalf("流式 notes = %#v, want 恰好一条工具签名说明", encoderNotes(t, enc))
	}

	resp := &ir.Response{Content: []ir.Block{*toolSigBlock("sig-call", codec.ProtocolGemini)}}
	_, nonStreamNotes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	nonStreamNote, m := firstToolSigNote(nonStreamNotes)
	if m != 1 {
		t.Fatalf("非流式 notes = %#v, want 恰好一条工具签名说明", nonStreamNotes)
	}
	if streamNote != nonStreamNote {
		t.Errorf("流式说明 %q 与非流式 %q 不一致——同一件事两种措辞会被当成两种故障",
			streamNote, nonStreamNote)
	}
}

func TestStreamEncoderSilentWithoutToolSignature(t *testing.T) {
	enc := inboundCodec{}.NewStreamEncoder(nil)
	encodeEvent(t, enc, ir.Event{
		Type: ir.EvBlockStart, Index: 0,
		Block: toolSigBlock("", ""),
	})
	if _, n := firstToolSigNote(encoderNotes(t, enc)); n != 0 {
		t.Errorf("notes = %#v, want 无工具签名说明", encoderNotes(t, enc))
	}
}
