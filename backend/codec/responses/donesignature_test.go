package responses

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 本文件守「收尾条目上的推理签名不能丢」。判据 1–7。
//
// 多数 responses 形态的上游只在 output_item.done 上挂 encrypted_content，
// added 那一帧里没有。此前解码器只在 added 上读它，于是签名整条丢失——而块
// 已被 done 正常闭合，UnsafeToClose 只看未闭合的块，所以既不报 incomplete
// 也不出任何说明。客户端把无签名的思考块存进历史，下一轮推理项接不上。

// feed 把若干 SSE 事件喂给解码器，返回聚合后的响应。
func feedEvents(t *testing.T, raw ...string) (*ir.Response, []string) {
	t.Helper()
	d := newStreamDecoder()
	var agg ir.Aggregator
	for _, r := range raw {
		evs, err := d.Feed("", r)
		if err != nil {
			t.Fatalf("Feed(%s): %v", r, err)
		}
		for _, ev := range evs {
			agg.Add(ev)
		}
	}
	for _, ev := range d.Finish() {
		agg.Add(ev)
	}
	resp := agg.Response()
	return resp, d.Notes()
}

func reasoningAdded(idx int, sig string) string {
	item := map[string]any{"type": "reasoning", "id": "rs_1"}
	if sig != "" {
		item["encrypted_content"] = sig
	}
	b, _ := json.Marshal(map[string]any{
		"type": "response.output_item.added", "output_index": idx, "item": item,
	})
	return string(b)
}

func reasoningDone(idx int, sig string) string {
	item := map[string]any{"type": "reasoning", "id": "rs_1"}
	if sig != "" {
		item["encrypted_content"] = sig
	}
	b, _ := json.Marshal(map[string]any{
		"type": "response.output_item.done", "output_index": idx, "item": item,
	})
	return string(b)
}

func thinkingBlock(t *testing.T, resp *ir.Response) *ir.Thinking {
	t.Helper()
	for _, b := range resp.Content {
		if b.Type == ir.BlockThinking && b.Thinking != nil {
			return b.Thinking
		}
	}
	t.Fatalf("响应里没有思考块：%+v", resp.Content)
	return nil
}

// 判据 1：done 带签名时块携带它，来源是本协议。
func TestSignatureOnItemDoneIsKept(t *testing.T) {
	resp, notes := feedEvents(t,
		reasoningAdded(0, ""),
		reasoningDone(0, "gAAAAAB-from-done"),
	)
	th := thinkingBlock(t, resp)
	if th.Signature != "gAAAAAB-from-done" {
		t.Errorf("签名 = %q，want 收尾条目那份——丢了它下一轮推理接不上", th.Signature)
	}
	// 判据 2
	if th.SignatureFrom != Name {
		t.Errorf("来源 = %q，want %q；来源为空会被下游按异族剥离", th.SignatureFrom, Name)
	}
	if len(notes) != 0 {
		t.Errorf("正常形态不该出说明：%#v", notes)
	}
}

// 判据 3：两帧都给且值相同时只留一份。
//
// 聚合器对签名增量是累加的，发两条会拼成一段无法解密的垃圾——那比丢一份更坏。
func TestSignatureOnBothFramesIsNotConcatenated(t *testing.T) {
	const sig = "gAAAAAB-same"
	resp, notes := feedEvents(t, reasoningAdded(0, sig), reasoningDone(0, sig))
	th := thinkingBlock(t, resp)
	if th.Signature != sig {
		t.Errorf("签名 = %q，want 单份 %q——两份密文拼起来上游解不开", th.Signature, sig)
	}
	if len(notes) != 0 {
		t.Errorf("同值不算异常：%#v", notes)
	}
}

// 判据 3 续：两帧给了不同的密文时留说明，值仍只有一份。
func TestConflictingSignaturesLeaveANote(t *testing.T) {
	resp, notes := feedEvents(t,
		reasoningAdded(0, "gAAAAAB-first"),
		reasoningDone(0, "gAAAAAB-second"),
	)
	th := thinkingBlock(t, resp)
	if th.Signature != "gAAAAAB-first" {
		t.Errorf("签名 = %q，want 先到的那份", th.Signature)
	}
	if len(notes) == 0 {
		t.Fatal("两帧给了不同密文却一声不响——这种形态运维必须看得见")
	}
	if !strings.Contains(notes[0], "two different reasoning signatures") {
		t.Errorf("说明没点明冲突：%q", notes[0])
	}
}

// 判据 4：签名必须排在 BlockStop 之前。
//
// 之后到的签名增量聚合器不收（那个块已不在 open 列里），会静默丢掉。
// 这条直接看事件序，不看聚合结果：顺序反了聚合结果就是「没有签名」，
// 与「上游没给」无法区分。
func TestSignatureEventPrecedesBlockStop(t *testing.T) {
	d := newStreamDecoder()
	if _, err := d.Feed("", reasoningAdded(0, "")); err != nil {
		t.Fatal(err)
	}
	evs, err := d.Feed("", reasoningDone(0, "gAAAAAB-x"))
	if err != nil {
		t.Fatal(err)
	}
	var sigAt, stopAt = -1, -1
	for i, ev := range evs {
		switch ev.Type {
		case ir.EvSigDelta:
			sigAt = i
		case ir.EvBlockStop:
			stopAt = i
		}
	}
	if sigAt < 0 {
		t.Fatalf("itemDone 没发签名事件：%+v", evs)
	}
	if stopAt < 0 {
		t.Fatalf("itemDone 没发闭块事件：%+v", evs)
	}
	if sigAt > stopAt {
		t.Errorf("签名排在闭块之后（sig=%d stop=%d），聚合器收不到它", sigAt, stopAt)
	}
}

// 判据 5：done 不带签名时保留 added 那份。
func TestSignatureOnlyOnAddedStillKept(t *testing.T) {
	resp, notes := feedEvents(t, reasoningAdded(0, "gAAAAAB-early"), reasoningDone(0, ""))
	th := thinkingBlock(t, resp)
	if th.Signature != "gAAAAAB-early" {
		t.Errorf("签名 = %q，want added 那份——补发逻辑不该抹掉已有的", th.Signature)
	}
	// 这条路径上没有补发事件，来源只能来自块开启帧那一处。判据 2 那条测的是
	// 补发事件带来源，两处各有一份来源看着冗余，但少任何一处都会让对应形态的
	// 签名被下游按异族剥离。
	if th.SignatureFrom != Name {
		t.Errorf("来源 = %q，want %q；块开启帧没填来源，这份签名会被下游剥离",
			th.SignatureFrom, Name)
	}
	if len(notes) != 0 {
		t.Errorf("不该出说明：%#v", notes)
	}
}

// 判据 5 续：块由推理增量开启（上游没发 output_item.added）时，
// 收尾条目的签名与来源都只能来自补发事件。
//
// 单独一条而不是并进判据 1：那条的块由 added 开启、来源已由块体带上，
// 于是「补发事件不填来源」在那条形态里看不出来。
func TestSignatureFromDeltaOpenedBlockCarriesSource(t *testing.T) {
	delta, _ := json.Marshal(map[string]any{
		"type": "response.reasoning_summary_text.delta", "output_index": 0,
		"summary_index": 0, "delta": "pondering",
	})
	resp, notes := feedEvents(t, string(delta), reasoningDone(0, "gAAAAAB-late"))

	th := thinkingBlock(t, resp)
	if th.Signature != "gAAAAAB-late" {
		t.Errorf("签名 = %q，want 收尾条目那份", th.Signature)
	}
	if th.SignatureFrom != Name {
		t.Errorf("来源 = %q，want %q；这条路径上补发事件是唯一的来源出处",
			th.SignatureFrom, Name)
	}
	if len(notes) != 0 {
		t.Errorf("不该出说明：%#v", notes)
	}
}

// 判据 6：两处都没签名时不报错，也不出说明。
func TestNoSignatureAnywhereIsNotAnError(t *testing.T) {
	resp, notes := feedEvents(t, reasoningAdded(0, ""), reasoningDone(0, ""))
	th := thinkingBlock(t, resp)
	if th.Signature != "" {
		t.Errorf("凭空造出了签名：%q", th.Signature)
	}
	if len(notes) != 0 {
		t.Errorf("上游没给不是异常：%#v", notes)
	}
}

// 判据 7：非 reasoning 条目的 done 不受影响。
//
// 一个带 encrypted_content 的 message 条目（形状上可能出现）不该让文本块
// 拿到一个签名——那一位在文本块上没有意义，写进去会让下游按异族剥离时
// 报一条无意义的说明。
func TestNonReasoningItemDoneIgnoresSignature(t *testing.T) {
	added, _ := json.Marshal(map[string]any{
		"type": "response.output_item.added", "output_index": 0,
		"item": map[string]any{"type": "message", "id": "msg_1"},
	})
	partAdded, _ := json.Marshal(map[string]any{
		"type": "response.content_part.added", "output_index": 0, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": ""},
	})
	delta, _ := json.Marshal(map[string]any{
		"type": "response.output_text.delta", "output_index": 0, "content_index": 0,
		"delta": "hi",
	})
	done, _ := json.Marshal(map[string]any{
		"type": "response.output_item.done", "output_index": 0,
		"item": map[string]any{"type": "message", "id": "msg_1",
			"encrypted_content": "gAAAAAB-stray"},
	})
	resp, notes := feedEvents(t, string(added), string(partAdded), string(delta), string(done))
	for _, b := range resp.Content {
		if b.Type == ir.BlockThinking {
			t.Errorf("message 条目造出了思考块：%+v", b)
		}
	}
	// 判据也要看文本块没被污染：去掉「只对 reasoning 条目补发」那个判据时，
	// 签名会作为增量落到这个文本块所在的 index 上，而聚合器收签名增量时会
	// 按 thinking 类型补开一个块——只断言「没有思考块」恰好能抓住它，但
	// 只有在补开这一步发生时才抓得住。直接断言文本块的内容完好更可靠。
	var text string
	for _, b := range resp.Content {
		if b.Type == ir.BlockText {
			text += b.Text
		}
	}
	if text != "hi" {
		t.Errorf("文本块被污染了：%q", text)
	}
	if len(resp.Content) != 1 {
		t.Errorf("一个 message 条目产出了 %d 个块：%+v", len(resp.Content), resp.Content)
	}
	// 文本块上不该挂推理签名。聚合器收签名增量时用的是「取已有块，没有才补开」，
	// 所以一条发错索引的签名会落到这个文本块的 Thinking 上——块数量与文本内容
	// 都不变，只有这一位变了。不断言它，「对所有条目都补发签名」就是绿的。
	for _, b := range resp.Content {
		if b.Type == ir.BlockText && b.Thinking != nil {
			t.Errorf("文本块被挂上了推理签名：%+v——它会随历史回到上游，"+
				"而那份密文对应的不是这个块", b.Thinking)
		}
	}
	if len(notes) != 0 {
		t.Errorf("不该出说明：%#v", notes)
	}
}
