package chatcompletions

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次47：官方 chat.completion.chunk 顶层带 moderation（moderated completions 的审核
// 结果回执，随独立的 moderation chunk 抵达），但**没有** metadata 字段。数据面对上游
// 一律流式，故 chat 上游的审核回执实际经流式 chunk 抵达；此前流式解码器从不读
// chunk.Moderation，回执被 json.Unmarshal 静默吞掉，与非流式路径（轮次46 已修）不对称。
// 这组测试钉住：流式解码攒下 moderation 随收尾帧交付并进聚合器；流式编码把它单独成帧
// 回写（即便客户端 suppress 了 usage 帧）；缺席不发明。metadata 流式无需处理——官方
// chunk 本就无此字段，属协议固有缺席，非网关丢弃。

// 流式解码：moderation 随独立 chunk 抵达，收尾 EvMessageDelta 交付，聚合器落进 IR。
func TestStreamModerationDecodedIntoIR(t *testing.T) {
	dec := newStreamDecoder()
	frames := []string{
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`,
		`{"id":"c1","model":"m","choices":[],"moderation":{"input":{"flagged":true},"output":{"flagged":false}}}`,
		doneSentinel,
	}
	var agg ir.Aggregator
	var delta *ir.Event
	for _, f := range frames {
		evs, err := dec.Feed("", f)
		if err != nil {
			t.Fatalf("feed %s: %v", f, err)
		}
		for i := range evs {
			agg.Add(evs[i])
			if evs[i].Type == ir.EvMessageDelta {
				delta = &evs[i]
			}
		}
	}
	if delta == nil || !strings.Contains(string(delta.Moderation), `"flagged":true`) {
		t.Fatalf("收尾 delta 没带 moderation：%+v", delta)
	}
	if got := agg.Response().ResponsesModeration; !strings.Contains(string(got), `"flagged":true`) {
		t.Fatalf("moderation 没随流式进 IR：%s", got)
	}
}

// 显式 null 不算回执：不把 4 字节字面量当成审核结果。
func TestStreamModerationExplicitNullIgnored(t *testing.T) {
	dec := newStreamDecoder()
	frames := []string{
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`,
		`{"id":"c1","model":"m","choices":[],"moderation":null}`,
		doneSentinel,
	}
	var delta *ir.Event
	for _, f := range frames {
		evs, err := dec.Feed("", f)
		if err != nil {
			t.Fatalf("feed %s: %v", f, err)
		}
		for i := range evs {
			if evs[i].Type == ir.EvMessageDelta {
				delta = &evs[i]
			}
		}
	}
	if delta != nil && len(delta.Moderation) != 0 {
		t.Fatalf("null 被当成了审核回执：%s", delta.Moderation)
	}
}

// 流式编码：moderation 随收尾帧抵达，编码器单独成帧回写（官方 moderation chunk 形态）。
func TestStreamEncoderEmitsModerationOnFinish(t *testing.T) {
	enc := newStreamEncoder()
	var joined []byte
	appendFrames := func(frames [][]byte, err error, what string) {
		t.Helper()
		if err != nil {
			t.Fatalf("Encode(%s): %v", what, err)
		}
		for _, f := range frames {
			joined = append(joined, f...)
		}
	}
	f1, err1 := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "c1", Model: "m"})
	appendFrames(f1, err1, "start")
	f2, err2 := enc.Encode(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "hi"})
	appendFrames(f2, err2, "content")
	f3, err3 := enc.Encode(ir.Event{Type: ir.EvMessageDelta,
		Moderation: json.RawMessage(`{"input":{"flagged":true}}`)})
	appendFrames(f3, err3, "delta")
	appendFrames(enc.Finish(), nil, "finish")

	if !strings.Contains(string(joined), `"moderation":{"input":{"flagged":true}}`) {
		t.Errorf("流式出站丢了 moderation：%s", joined)
	}
}

// 整份响应投影路径（上游忽略 stream:true）：ResponseEvents 把 ResponsesModeration
// 随首帧 EvMessageStart 投影，编码器在 start 收下并写回收尾帧。
func TestStreamEncoderEmitsModerationFromReplay(t *testing.T) {
	resp := &ir.Response{ID: "c1", Model: "m", StopReason: ir.StopEndTurn,
		ResponsesModeration: json.RawMessage(`{"output":{"flagged":true}}`)}
	enc := newStreamEncoder()
	var joined []byte
	for _, ev := range ir.ResponseEvents(resp) {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		for _, f := range frames {
			joined = append(joined, f...)
		}
	}
	for _, f := range enc.Finish() {
		joined = append(joined, f...)
	}
	if !strings.Contains(string(joined), `"moderation":{"output":{"flagged":true}}`) {
		t.Errorf("投影路径丢了 moderation：%s", joined)
	}
}

// 客户端 suppress 了单独 usage 帧时，moderation 仍须单独成帧送达（不依附 usage 帧）。
func TestStreamEncoderEmitsModerationWhenUsageSuppressed(t *testing.T) {
	enc := newStreamEncoder()
	enc.suppressUsageFrame = true
	var joined []byte
	enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "c1", Model: "m"})
	enc.Encode(ir.Event{Type: ir.EvMessageDelta,
		Moderation: json.RawMessage(`{"input":{"flagged":false}}`)})
	frames := enc.Finish()
	for _, f := range frames {
		joined = append(joined, f...)
	}
	if strings.Contains(string(joined), `"usage"`) {
		t.Errorf("suppress 了却仍发 usage 帧：%s", joined)
	}
	if !strings.Contains(string(joined), `"moderation":{"input":{"flagged":false}}`) {
		t.Errorf("suppress usage 帧时 moderation 也丢了：%s", joined)
	}
}

// 缺席不发明：没有 moderation 的流，出站不该凭空多出 moderation 键。
func TestNoStreamModerationWhenAbsent(t *testing.T) {
	enc := newStreamEncoder()
	var joined []byte
	addFrames := func(frames [][]byte, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		for _, f := range frames {
			joined = append(joined, f...)
		}
	}
	f1, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "c1", Model: "m"})
	addFrames(f1, err)
	f2, err := enc.Encode(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "hi"})
	addFrames(f2, err)
	f3, err := enc.Encode(ir.Event{Type: ir.EvMessageDelta})
	addFrames(f3, err)
	addFrames(enc.Finish(), nil)
	if strings.Contains(string(joined), "moderation") {
		t.Errorf("缺席却写了 moderation 键：%s", joined)
	}
}
