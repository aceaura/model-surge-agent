package chatcompletions

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// chat 拒绝正文保真（移植旧仓 #11）。message.refusal / delta.refusal 是
// 本族专属槽位：解码进独立的 BlockRefusal（不并入文本），编码原样回槽位。

// 请求侧：assistant 历史里的 refusal 解成独立块，同族往返回到 refusal 键。
func TestRefusalRequestRoundTrip(t *testing.T) {
	body := `{"model":"m","messages":[` +
		`{"role":"user","content":"do it"},` +
		`{"role":"assistant","content":null,"refusal":"I cannot help with that."}]}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	blocks := req.Messages[1].Content
	if len(blocks) != 1 || blocks[0].Type != ir.BlockRefusal ||
		blocks[0].Text != "I cannot help with that." {
		t.Fatalf("拒绝块 = %+v", blocks)
	}

	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(out), `"refusal":"I cannot help with that."`) {
		t.Fatalf("refusal 未回槽位: %s", out)
	}
	// content 为 null 的拒绝消息不得被编成普通正文。
	if strings.Contains(string(out), `"I cannot help with that.","`) ||
		strings.Contains(string(out), `"content":"I cannot`) {
		t.Fatalf("拒绝正文混进了 content: %s", out)
	}
}

// 非流式响应：官方拒绝形态是 content=null + refusal 正文，两处都要收下。
func TestRefusalResponseDecode(t *testing.T) {
	body := `{"id":"c1","model":"m","choices":[{"index":0,"message":` +
		`{"role":"assistant","content":null,"refusal":"no"},"finish_reason":"stop"}]}`
	resp, _, err := DecodeResponseLossy([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != ir.BlockRefusal ||
		resp.Content[0].Text != "no" {
		t.Fatalf("content = %+v", resp.Content)
	}

	// 客户端编码回 refusal 键。
	out, notes, err := inboundCodec{}.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if len(notes) != 0 {
		t.Fatalf("本族接得住，误报: %v", notes)
	}
	if !strings.Contains(string(out), `"refusal":"no"`) {
		t.Fatalf("refusal 未回槽位: %s", out)
	}
	// 再解码无漂移。
	again, _, err := DecodeResponseLossy(out)
	if err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if len(again.Content) != 1 || again.Content[0].Type != ir.BlockRefusal ||
		again.Content[0].Text != "no" {
		t.Fatalf("再解码漂移: %+v", again.Content)
	}
}

// 流式解码：delta.refusal 自成一块，不并入 delta.content 的文本槽位。
func TestRefusalStreamDecode(t *testing.T) {
	resp, _ := decodeStream(t,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"refusal":"我不能"}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"refusal":"这么做"}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}]}`,
		doneSentinel,
	)
	if len(resp.Content) != 1 {
		t.Fatalf("content = %+v", resp.Content)
	}
	b := resp.Content[0]
	if b.Type != ir.BlockRefusal || b.Text != "我不能这么做" {
		t.Fatalf("拒绝块 = %+v", b)
	}
}

// 流式编码（客户端方向）：拒绝块的增量走 delta.refusal 而不是 delta.content。
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
	feed(ir.Event{Type: ir.EvMessageStart, MessageID: "c1", Model: "m"})
	feed(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockRefusal}})
	got := feed(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "no"})
	feed(ir.Event{Type: ir.EvBlockStop, Index: 0})
	feed(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopContentFilter})
	feed(ir.Event{Type: ir.EvMessageStop})

	if !strings.Contains(got, `"delta":{"refusal":"no"}`) {
		t.Fatalf("增量没走 delta.refusal: %s", got)
	}
	if strings.Contains(got, `"content"`) {
		t.Fatalf("拒绝增量混进了 content: %s", got)
	}
}
