package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次16 R16-1：缓存写入 TTL 明细与 inference_geo 只有 message_start 的完整
// usage 能承载（renderUsage 写），message_delta 的官方 MessageDeltaUsage 没有
// 这两个槽位（renderDeltaUsage 不写）。真流式它们随起始帧到达、已下发，不报；
// 但「上游忽略 stream:true 回整份 JSON」的投影路径（ir.ResponseEvents）把全部
// 用量压在 EvMessageDelta、message_start 不带用量，明细与地理就送不出去——与
// 非流式分叉，此前静默。这组测试钉住「仅起始帧没兜住时才报」的判据。

const lateUsageNote = "arrived on the stream's closing usage frame"

// startOnly 走真流式形态：明细 + 地理随 message_start 到达。
func TestR16GenuineStreamStartDeliversNoNote(t *testing.T) {
	enc, s := runStream(t,
		ir.Event{Type: ir.EvMessageStart, MessageID: "m1", Model: "m", Usage: &ir.Usage{
			InputTokens: 70, CacheWriteTokens: 25, CacheWrite5mTokens: 10,
			CacheWrite1hTokens: 15, CacheWriteDetailsKnown: true, InferenceGeo: "us"}},
		ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Usage: &ir.Usage{OutputTokens: 20}},
	)
	if !strings.Contains(s, `"cache_creation":{`) || !strings.Contains(s, `"inference_geo":"us"`) {
		t.Fatalf("起始帧没写出明细/地理：%s", s)
	}
	if anyHas(enc.Notes(), lateUsageNote) {
		t.Errorf("真流式（明细随起始帧下发）被误报：%v", enc.Notes())
	}
}

// replayOnly 走投影形态：起始帧无用量，全部明细压在收尾帧。两维都该报。
func TestR16ReplayDeltaOnlyNotesBothDims(t *testing.T) {
	enc, _ := runStream(t,
		ir.Event{Type: ir.EvMessageStart, MessageID: "m1", Model: "m"},
		ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Usage: &ir.Usage{
			InputTokens: 70, OutputTokens: 20, CacheWriteTokens: 25,
			CacheWrite5mTokens: 10, CacheWrite1hTokens: 15,
			CacheWriteDetailsKnown: true, InferenceGeo: "us"}},
	)
	notes := enc.Notes()
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "cache-creation TTL details") {
		t.Errorf("投影路径丢了缓存 TTL 明细却没报：%v", notes)
	}
	if !strings.Contains(joined, "inference geo") {
		t.Errorf("投影路径丢了 inference_geo 却没报：%v", notes)
	}
	if !anyHas(notes, lateUsageNote) {
		t.Errorf("注记没落在「随收尾帧到达」的真实处置上：%v", notes)
	}
}

// 起始帧已兜住明细但没带地理，收尾帧补来地理：只报地理，不报明细。
func TestR16PartialStartDeliveredNotesOnlyMissing(t *testing.T) {
	enc, _ := runStream(t,
		ir.Event{Type: ir.EvMessageStart, MessageID: "m1", Model: "m", Usage: &ir.Usage{
			InputTokens: 70, CacheWriteTokens: 25, CacheWrite5mTokens: 10,
			CacheWrite1hTokens: 15, CacheWriteDetailsKnown: true}},
		ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Usage: &ir.Usage{
			OutputTokens: 20, InferenceGeo: "us"}},
	)
	notes := enc.Notes()
	joined := strings.Join(notes, "\n")
	if strings.Contains(joined, "cache-creation TTL details") {
		t.Errorf("明细已随起始帧下发，不该再报：%v", notes)
	}
	if !strings.Contains(joined, "inference geo") {
		t.Errorf("地理只在收尾帧、起始帧没带，应报出：%v", notes)
	}
}

// 收尾帧只带聚合总量、无明细无地理：不许凭空注记。
func TestR16NoDetailsNoNote(t *testing.T) {
	enc, _ := runStream(t,
		ir.Event{Type: ir.EvMessageStart, MessageID: "m1", Model: "m"},
		ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Usage: &ir.Usage{
			InputTokens: 70, OutputTokens: 20, CacheWriteTokens: 25}},
	)
	if anyHas(enc.Notes(), lateUsageNote) {
		t.Errorf("无明细无地理却报出收尾帧损耗：%v", enc.Notes())
	}
}

// 抽干式：报过一次后二次调用不重复。
func TestR16LateUsageNoteDrainedOnce(t *testing.T) {
	enc, _ := runStream(t,
		ir.Event{Type: ir.EvMessageStart, MessageID: "m1", Model: "m"},
		ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Usage: &ir.Usage{
			CacheWriteTokens: 25, CacheWrite5mTokens: 10, CacheWrite1hTokens: 15,
			CacheWriteDetailsKnown: true, InferenceGeo: "us"}},
	)
	if first := enc.Notes(); !anyHas(first, lateUsageNote) {
		t.Fatalf("首取没报出：%v", first)
	}
	if second := enc.Notes(); anyHas(second, lateUsageNote) {
		t.Errorf("同一损耗被重复报出：%v", second)
	}
}

// runStream 顺序喂事件、收尾，返回编码器与已产出的线格式。
func runStream(t *testing.T, events ...ir.Event) (*streamEncoder, string) {
	t.Helper()
	enc := newStreamEncoder()
	var wire strings.Builder
	for _, ev := range events {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("Encode(%s): %v", ev.Type, err)
		}
		for _, f := range frames {
			wire.Write(f)
		}
	}
	for _, f := range enc.Finish() {
		wire.Write(f)
	}
	return enc, wire.String()
}

func anyHas(notes []string, sub string) bool {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}
