package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次49：上游审核回执（ResponsesModeration）与客户端关联键值回声
//（ClientMetadata）是 chat/responses 两族专属的响应级槽位。anthropic 响应没有
// moderation / metadata 字段，跨族投影（client=anthropic、上游=chat/responses）
// 来的一律无处安放。此前非流式 EncodeResponseLossy 与流式 Notes() 两条路径都
// 静默丢弃——与 ContainerDropNote 正好互为镜像（那是 anthropic 专属回显被外族
// 丢、外族两条路径都报），请求侧同类丢弃也早已报出，唯独响应侧分叉。这里补齐。
//
// moderation 是上游内容安全侧产出、不是客户端回声，对 anthropic 客户端真实可达；
// metadata 是纯回声，anthropic 客户端请求无从设置，通常不可达，故其注记是对称
// 防御——仅字段真非空时报出，绝不空值误报。

const modNote = "moderation receipt"
const metaNote = "echoed client metadata"

// 非流式：moderation 回执丢弃要报。
func TestR49NonStreamModerationNoted(t *testing.T) {
	_, notes, err := inboundCodec{}.EncodeResponseLossy(&ir.Response{
		ID: "msg_1", Model: "m", StopReason: ir.StopEndTurn,
		ResponsesModeration: json.RawMessage(`{"input":{"flagged":true}}`),
	})
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if !hasNote(notes, modNote) {
		t.Errorf("非流式 moderation 丢弃未报：%v", notes)
	}
}

// 非流式：客户端关联键值回声丢弃要报。
func TestR49NonStreamMetadataNoted(t *testing.T) {
	_, notes, err := inboundCodec{}.EncodeResponseLossy(&ir.Response{
		ID: "msg_1", Model: "m", StopReason: ir.StopEndTurn,
		ClientMetadata: map[string]string{"trace": "abc"},
	})
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if !hasNote(notes, metaNote) {
		t.Errorf("非流式 metadata 回声丢弃未报：%v", notes)
	}
}

// 非流式：显式 null 的 moderation 不算到达，不报（与非流式判据一致）。
func TestR49NonStreamExplicitNullModerationSilent(t *testing.T) {
	_, notes, err := inboundCodec{}.EncodeResponseLossy(&ir.Response{
		ID: "msg_1", Model: "m", StopReason: ir.StopEndTurn,
		ResponsesModeration: json.RawMessage(`null`),
	})
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if hasNote(notes, modNote) {
		t.Errorf("显式 null moderation 被误报：%v", notes)
	}
}

// 非流式：两者皆无时不报。
func TestR49NonStreamAbsentSilent(t *testing.T) {
	_, notes, err := inboundCodec{}.EncodeResponseLossy(&ir.Response{
		ID: "msg_1", Model: "m", StopReason: ir.StopEndTurn,
	})
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if hasNote(notes, modNote) || hasNote(notes, metaNote) {
		t.Errorf("空响应被误报 metadata/moderation 丢弃：%v", notes)
	}
}

// 流式：moderation 随 message_start 到达 → Notes() 报出。
func TestR49StreamModerationNoted(t *testing.T) {
	enc := newStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "msg_1", Model: "m",
		Moderation: json.RawMessage(`{"output":{"flagged":true}}`)}); err != nil {
		t.Fatalf("Encode(start): %v", err)
	}
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn}); err != nil {
		t.Fatalf("Encode(delta): %v", err)
	}
	enc.Finish()
	if !hasNote(enc.Notes(), modNote) {
		t.Errorf("流式 moderation 丢弃未报：%v", enc.Notes())
	}
}

// 流式：metadata 随 message_delta 到达 → Notes() 报出。
func TestR49StreamMetadataNoted(t *testing.T) {
	enc := newStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "msg_1", Model: "m"}); err != nil {
		t.Fatalf("Encode(start): %v", err)
	}
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn,
		Metadata: map[string]string{"trace": "abc"}}); err != nil {
		t.Fatalf("Encode(delta): %v", err)
	}
	enc.Finish()
	if !hasNote(enc.Notes(), metaNote) {
		t.Errorf("流式 metadata 回声丢弃未报：%v", enc.Notes())
	}
}

// 流式：显式 null 的 moderation 不算到达，不报。
func TestR49StreamExplicitNullModerationSilent(t *testing.T) {
	enc := newStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "msg_1", Model: "m",
		Moderation: json.RawMessage(`null`)}); err != nil {
		t.Fatalf("Encode(start): %v", err)
	}
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn}); err != nil {
		t.Fatalf("Encode(delta): %v", err)
	}
	enc.Finish()
	if hasNote(enc.Notes(), modNote) {
		t.Errorf("流式显式 null moderation 被误报：%v", enc.Notes())
	}
}

// 流式：两者皆无时不报。
func TestR49StreamAbsentSilent(t *testing.T) {
	enc := newStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "msg_1", Model: "m"}); err != nil {
		t.Fatalf("Encode(start): %v", err)
	}
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn}); err != nil {
		t.Fatalf("Encode(delta): %v", err)
	}
	enc.Finish()
	if hasNote(enc.Notes(), modNote) || hasNote(enc.Notes(), metaNote) {
		t.Errorf("流式空响应被误报：%v", enc.Notes())
	}
}

// 流式：报出即抽干，Notes() 二次调用不重复。
func TestR49StreamDrainedAfterReport(t *testing.T) {
	enc := newStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "msg_1", Model: "m",
		Moderation: json.RawMessage(`{"input":{"flagged":false}}`),
		Metadata:   map[string]string{"k": "v"}}); err != nil {
		t.Fatalf("Encode(start): %v", err)
	}
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn}); err != nil {
		t.Fatalf("Encode(delta): %v", err)
	}
	enc.Finish()
	if first := enc.Notes(); !hasNote(first, modNote) || !hasNote(first, metaNote) {
		t.Fatalf("首次 Notes() 未报全：%v", first)
	}
	if second := enc.Notes(); hasNote(second, modNote) || hasNote(second, metaNote) {
		t.Errorf("二次 Notes() 重复报出：%v", second)
	}
}

// 整份响应投影路径（上游非流式响应 → 客户端流式）：ir.ResponseEvents 把
// ResponsesModeration / ClientMetadata 投到 EvMessageStart，同样要报出——
// 真流式与投影两条路径不许口径分叉。
func TestR49ProjectionPathNoted(t *testing.T) {
	resp := &ir.Response{
		ID: "msg_1", Model: "m", StopReason: ir.StopEndTurn,
		ResponsesModeration: json.RawMessage(`{"input":{"flagged":true}}`),
		ClientMetadata:      map[string]string{"trace": "abc"},
	}
	enc := newStreamEncoder()
	for _, ev := range ir.ResponseEvents(resp) {
		if _, err := enc.Encode(ev); err != nil {
			t.Fatalf("Encode(%v): %v", ev.Type, err)
		}
	}
	enc.Finish()
	notes := enc.Notes()
	if !hasNote(notes, modNote) {
		t.Errorf("投影路径 moderation 丢弃未报：%v", notes)
	}
	if !hasNote(notes, metaNote) {
		t.Errorf("投影路径 metadata 回声丢弃未报：%v", notes)
	}
}
