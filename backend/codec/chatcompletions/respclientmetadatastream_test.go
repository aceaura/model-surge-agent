package chatcompletions

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次55：chat_completions 流式编码器此前静默丢弃客户端关联键值回声（ev.Metadata）。
// 官方 chat.completion.chunk（CreateChatCompletionStreamResponse）顶层无 metadata 字段
// ——metadata 只在非流式 CreateChatCompletionResponse 上（spec 核实）。故当 metadata 由
// responses 上游回显、经 EvMessageStart / EvMessageDelta 投影到 chat 流式编码器时，chunk
// 装不下、必然丢弃；而同一编码器的 prompt_cache_diagnostics 丢弃、以及 anthropic 流式
// 编码器的 metadata 丢弃都照实报出，chat 流式却漏报——违反「never silently drop」。
// 判据与 ResponseClientMetadataDropNote 同源，指纹 = "echoed client metadata"。

const clientMetaFingerprint = "echoed client metadata"

// 首帧抵达（整份响应投影形态）：EvMessageStart 带 Metadata → Notes() 报出。
func TestChatStreamClientMetadataNoted(t *testing.T) {
	enc := newStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "c1", Model: "m",
		Metadata: map[string]string{"trace": "abc"}}); err != nil {
		t.Fatalf("Encode(start): %v", err)
	}
	enc.Encode(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "hi"})
	enc.Encode(ir.Event{Type: ir.EvMessageDelta})
	enc.Finish()
	if !anyNoteHas(enc.Notes(), clientMetaFingerprint) {
		t.Errorf("流式 chat 未报出客户端 metadata 丢弃：%v", enc.Notes())
	}
}

// 收尾帧抵达（真流式形态）：EvMessageDelta 带 Metadata → 第二个捕获点同样报出。
func TestChatStreamClientMetadataOnDeltaNoted(t *testing.T) {
	enc := newStreamEncoder()
	enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "c1", Model: "m"})
	enc.Encode(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "hi"})
	enc.Encode(ir.Event{Type: ir.EvMessageDelta, Metadata: map[string]string{"idempotency": "k1"}})
	enc.Finish()
	if !anyNoteHas(enc.Notes(), clientMetaFingerprint) {
		t.Errorf("收尾帧抵达的 metadata 未报出：%v", enc.Notes())
	}
}

// 整份响应投影路径：ir.ResponseEvents 把 ClientMetadata 投到首个 EvMessageStart，
// 编码器收下并于 Notes() 报出（与 moderation 投影测试同款结构）。
func TestChatStreamClientMetadataFromReplay(t *testing.T) {
	resp := &ir.Response{ID: "c1", Model: "m", StopReason: ir.StopEndTurn,
		ClientMetadata: map[string]string{"trace": "abc"}}
	enc := newStreamEncoder()
	for _, ev := range ir.ResponseEvents(resp) {
		if _, err := enc.Encode(ev); err != nil {
			t.Fatalf("Encode: %v", err)
		}
	}
	enc.Finish()
	if !anyNoteHas(enc.Notes(), clientMetaFingerprint) {
		t.Errorf("投影路径丢了 metadata 且未报出：%v", enc.Notes())
	}
}

// 缺席不发明：没有 metadata 的流，Notes() 不该凭空多出该注记。
func TestChatStreamClientMetadataAbsentSilent(t *testing.T) {
	enc := newStreamEncoder()
	enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "c1", Model: "m"})
	enc.Encode(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "hi"})
	enc.Encode(ir.Event{Type: ir.EvMessageDelta})
	enc.Finish()
	if anyNoteHas(enc.Notes(), clientMetaFingerprint) {
		t.Errorf("缺席却报了 metadata 注记：%v", enc.Notes())
	}
}

// 抽干纪律：Notes() 报出即清，二次调用不重复。
func TestChatStreamClientMetadataDrainedOnSecondCall(t *testing.T) {
	enc := newStreamEncoder()
	enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "c1", Model: "m",
		Metadata: map[string]string{"trace": "abc"}})
	enc.Finish()
	if !anyNoteHas(enc.Notes(), clientMetaFingerprint) {
		t.Fatalf("首次 Notes() 未报出：%v", enc.Notes())
	}
	if anyNoteHas(enc.Notes(), clientMetaFingerprint) {
		t.Errorf("二次 Notes() 重复报出（未抽干）：%v", enc.Notes())
	}
}

// 同族非流式仍是 metadata 承载方：EncodeResponseLossy 把 ClientMetadata 写回 wireResponse
// 且不报丢弃——钉住「流式补注记、非流式保全」的差异是有意为之（chunk 无 metadata 槽位、
// 非流式响应有），不因本轮给流式补注记而误伤非流式保全。
func TestChatNonStreamStillCarriesMetadata(t *testing.T) {
	resp := &ir.Response{ID: "c1", Model: "m", StopReason: ir.StopEndTurn,
		ClientMetadata: map[string]string{"trace": "abc"}}
	body, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if !strings.Contains(string(body), `"metadata"`) {
		t.Errorf("非流式 chat 未回写 metadata：%s", body)
	}
	for _, n := range notes {
		if strings.Contains(n, clientMetaFingerprint) {
			t.Errorf("非流式 chat 是 metadata 承载方，不该报丢弃：%v", n)
		}
	}
}
