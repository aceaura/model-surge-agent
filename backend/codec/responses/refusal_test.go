package responses

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// responses 拒绝正文保真（移植旧仓 #11）。type=refusal content part 与
// response.refusal.delta/.done 是本族专属槽位：解码进独立的 BlockRefusal，
// 编码原样回槽位，同族往返不降级。

// 请求侧：历史里的拒绝块编回 refusal part，解码无漂移。
func TestRefusalRequestRoundTrip(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 16, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "do it"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{
			{Type: ir.BlockRefusal, Text: "I cannot help with that."}}},
	}}
	body, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(body), `"type":"refusal"`) ||
		!strings.Contains(string(body), `"refusal":"I cannot help with that."`) {
		t.Fatalf("refusal part 缺席: %s", body)
	}

	again, err := DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	blocks := again.Messages[1].Content
	if len(blocks) != 1 || blocks[0].Type != ir.BlockRefusal ||
		blocks[0].Text != "I cannot help with that." {
		t.Fatalf("往返漂移: %+v", blocks)
	}
}

// 非流式响应编码：拒绝块进 refusal part，不落 default 报错。
func TestRefusalResponseEncode(t *testing.T) {
	resp := &ir.Response{ID: "r1", Model: "m", StopReason: ir.StopContentFilter,
		Content: []ir.Block{{Type: ir.BlockRefusal, Text: "no"}}}
	body, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if !strings.Contains(string(body), `"type":"refusal","refusal":"no"`) {
		t.Fatalf("refusal part 缺席: %s", body)
	}
	if strings.Contains(string(body), `"output_text"`) {
		t.Fatalf("拒绝被编成 output_text: %s", body)
	}
}

// 流式编码（客户端方向）：拒绝块走专属事件链——content_part.added(refusal)
// → response.refusal.delta → response.refusal.done → content_part.done(refusal)
// → output_item.done 里的 message content 也是 refusal part。
func TestRefusalStreamEncode(t *testing.T) {
	enc := newStreamEncoder()
	feed := func(ev ir.Event) string {
		out, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("encode %v: %v", ev.Type, err)
		}
		var sb strings.Builder
		for _, f := range out {
			sb.Write(f)
		}
		return sb.String()
	}
	feed(ir.Event{Type: ir.EvMessageStart, MessageID: "r1", Model: "m"})
	added := feed(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockRefusal}})
	delta := feed(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "我不能"})
	stop := feed(ir.Event{Type: ir.EvBlockStop, Index: 0})
	feed(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopContentFilter})
	feed(ir.Event{Type: ir.EvMessageStop})

	if !strings.Contains(added, `"type":"refusal"`) {
		t.Fatalf("content_part.added 缺 refusal part: %s", added)
	}
	if !strings.Contains(delta, `"response.refusal.delta"`) ||
		!strings.Contains(delta, `"delta":"我不能"`) {
		t.Fatalf("增量没走 refusal.delta: %s", delta)
	}
	if strings.Contains(delta, `"response.output_text.delta"`) {
		t.Fatalf("拒绝增量走了 output_text.delta: %s", delta)
	}
	// refusal.done 终态必须带全量文本（字母序重排后 refusal 在 type 之前）。
	if !strings.Contains(stop, `"refusal":"我不能","type":"response.refusal.done"`) {
		t.Fatalf("关块缺 refusal.done: %s", stop)
	}
	// 顶层键被 backfillIndexFields 按字母序重排，事件名断言不能带前导引号。
	if !strings.Contains(stop, `response.content_part.done`) ||
		!strings.Contains(stop, `"type":"refusal","refusal":"我不能"`) {
		t.Fatalf("content_part.done 缺 refusal 终态: %s", stop)
	}
	if !strings.Contains(stop, `response.output_item.done`) ||
		strings.Count(stop, `"type":"refusal"`) < 2 {
		t.Fatalf("output_item.done 的 message content 缺 refusal part: %s", stop)
	}
}
