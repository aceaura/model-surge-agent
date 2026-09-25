package codec_test

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	_ "github.com/aceaura/model-surge-agent/backend/codec/anthropic"
	_ "github.com/aceaura/model-surge-agent/backend/codec/chatcompletions"
	_ "github.com/aceaura/model-surge-agent/backend/codec/gemini"
	_ "github.com/aceaura/model-surge-agent/backend/codec/responses"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// #41 Anthropic 缓存写入 TTL 明细的跨族保真。
//
// cache_creation 的 5m/1h 细分只有 anthropic 的响应有槽位，两档单价不同
// （1h 通常 2×、5m 1.25×），只有合计做不了成本归因。四件事分别钉住：
// 同族解码收下明细（含「只给细分不给合计」时补齐合计）、同族编码照实
// 写出（已知明细为真零也写，未知不伪造）、跨族丢弃必须报出且只报一次、
// 合计本身跨族不丢。
//
// 「明细已知」标记是这组断言的支点：没有它，全零明细与「上游没给」同形，
// 出站要么伪造精度要么丢掉真零。

func cacheDetailsUsage() ir.Usage {
	return ir.Usage{
		InputTokens: 100, OutputTokens: 5, CacheReadTokens: 40,
		CacheWriteTokens: 30, CacheWrite5mTokens: 30, CacheWrite1hTokens: 0,
		CacheWriteDetailsKnown: true,
	}
}

const cacheDetailsNote = "cache-creation TTL details"

func hasCacheDetailsNote(notes []string) bool {
	return hasNote(notes, cacheDetailsNote)
}

func TestAnthropicDecodesCacheCreationDetails(t *testing.T) {
	body := []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude",` +
		`"content":[],"stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":5,` +
		`"cache_read_input_tokens":40,"cache_creation_input_tokens":30,` +
		`"cache_creation":{"ephemeral_5m_input_tokens":20,"ephemeral_1h_input_tokens":10}}}`)
	c, ok := codec.Outbound(codec.ProtocolAnthropic)
	if !ok {
		t.Fatal("anthropic outbound 未注册")
	}
	resp, err := c.DecodeResponse(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	u := resp.Usage
	if u.CacheWriteTokens != 30 || u.CacheWrite5mTokens != 20 ||
		u.CacheWrite1hTokens != 10 || !u.CacheWriteDetailsKnown {
		t.Fatalf("TTL 明细没解进 IR：%+v", u)
	}
}

// 上游只给细分不给合计的形态：合计用两档之和补齐。记账侧只认合计字段，
// 不补就等于把这笔缓存写入丢了。
func TestAnthropicDerivesAggregateFromCacheCreationDetails(t *testing.T) {
	body := []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude",` +
		`"content":[],"stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":5,` +
		`"cache_creation":{"ephemeral_5m_input_tokens":20,"ephemeral_1h_input_tokens":10}}}`)
	c, _ := codec.Outbound(codec.ProtocolAnthropic)
	resp, err := c.DecodeResponse(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Usage.CacheWriteTokens != 30 {
		t.Errorf("合计 = %d，想要 20+10=30", resp.Usage.CacheWriteTokens)
	}
	if !resp.Usage.CacheWriteDetailsKnown {
		t.Errorf("明细到了却没置已知标记：%+v", resp.Usage)
	}
}

// 流式同口径：明细在 message_start 帧到达，聚合后不能丢。
func TestAnthropicStreamDecodesCacheCreationDetails(t *testing.T) {
	raw := `data: {"type":"message_start","message":{"id":"msg_1","model":"claude","role":"assistant","usage":{"input_tokens":100,"cache_read_input_tokens":40,"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":20,"ephemeral_1h_input_tokens":10}}}}` + "\n\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}` + "\n\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	var agg ir.Aggregator
	for _, ev := range decodeStream(t, codec.ProtocolAnthropic, raw) {
		agg.Add(ev)
	}
	u := agg.Response().Usage
	if u.CacheWriteTokens != 30 || u.CacheWrite5mTokens != 20 ||
		u.CacheWrite1hTokens != 10 || !u.CacheWriteDetailsKnown || u.OutputTokens != 5 {
		t.Fatalf("流式聚合丢了 TTL 明细：%+v", u)
	}
}

// 同族编码：已知明细照实写出，真零的 1h 档也写键（内层无 omitempty——
// 上游回这个对象时两键总是同时出现，隐去零值反而像明细残缺）；
// 未知明细不写对象，伪造精度比丢精度更坏。
func TestAnthropicEncodesCacheCreationDetails(t *testing.T) {
	c, ok := codec.Inbound(codec.ProtocolAnthropic)
	if !ok {
		t.Fatal("anthropic inbound 未注册")
	}
	resp := &ir.Response{ID: "msg_1", Model: "claude", StopReason: ir.StopEndTurn,
		Content: []ir.Block{{Type: ir.BlockText, Text: "ok"}}, Usage: cacheDetailsUsage()}
	body, err := c.EncodeResponse(resp)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for _, want := range []string{
		`"cache_creation_input_tokens":30`,
		`"cache_creation":{"ephemeral_5m_input_tokens":30,"ephemeral_1h_input_tokens":0}`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("响应缺 %s：%s", want, body)
		}
	}

	// 未知明细：合计照写，细分对象不出现。
	unknown := *resp
	unknown.Usage = ir.Usage{InputTokens: 100, OutputTokens: 5, CacheWriteTokens: 30}
	body2, err := c.EncodeResponse(&unknown)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if strings.Contains(string(body2), `"cache_creation":`) {
		t.Errorf("明细未知却写出了 cache_creation 对象：%s", body2)
	}
	if !strings.Contains(string(body2), `"cache_creation_input_tokens":30`) {
		t.Errorf("合计不该跟着明细一起消失：%s", body2)
	}
}

// 同族流式：首帧带明细，输出里要有 cache_creation 且不得报丢弃。
func TestAnthropicStreamPreservesCacheCreationDetails(t *testing.T) {
	ic, _ := codec.Inbound(codec.ProtocolAnthropic)
	enc := ic.NewStreamEncoder(nil)
	u := cacheDetailsUsage()
	out := encodeAll(t, enc, []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "m1", Model: "claude", Usage: &u},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Usage: &ir.Usage{OutputTokens: 5}},
		{Type: ir.EvMessageStop},
	})
	if !strings.Contains(out, `"ephemeral_5m_input_tokens":30`) ||
		!strings.Contains(out, `"ephemeral_1h_input_tokens":0`) {
		t.Errorf("anthropic 流式丢了 TTL 明细：%s", out)
	}
	if hasCacheDetailsNote(enc.(codec.StreamNotes).Notes()) {
		t.Errorf("anthropic 自家接得住，误报：%v", enc.(codec.StreamNotes).Notes())
	}
}

// 跨族流式：明细丢弃要报、且两帧都带明细也只报一次；合计不陪葬。
func TestForeignStreamsReportCacheDetailsLossOnce(t *testing.T) {
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses} {
		ic, _ := codec.Inbound(name)
		enc := ic.NewStreamEncoder(nil)
		u := cacheDetailsUsage()
		late := cacheDetailsUsage()
		out := encodeAll(t, enc, []ir.Event{
			{Type: ir.EvMessageStart, MessageID: "m1", Model: "m", Usage: &u},
			{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Usage: &late},
			{Type: ir.EvMessageStop},
		})
		notes := enc.(codec.StreamNotes).Notes()
		if n := strings.Count(strings.Join(notes, "; "), cacheDetailsNote); n != 1 {
			t.Errorf("%s 明细丢弃注记应恰一条，实得 %d：%v", name, n, notes)
		}
		// 合计与输入总量不陪葬：chat 有兼容层别名字段，responses 的
		// input_tokens 含缓存命中（100+40）。
		switch name {
		case codec.ProtocolChatCompletions:
			if !strings.Contains(out, `"cache_write_tokens":30`) {
				t.Errorf("%s 缓存写入合计丢了：%s", name, out)
			}
		case codec.ProtocolResponses:
			if !strings.Contains(out, `"input_tokens":140`) {
				t.Errorf("%s 输入总量丢了：%s", name, out)
			}
		}
	}
}

// 跨族非流式：三通道里 EncodeResponseLossy 这一条也要报；
// anthropic 自家与「明细未知」全静默。
func TestCacheDetailsLossNotesOnWholeResponses(t *testing.T) {
	resp := &ir.Response{ID: "m1", Model: "m",
		Content: []ir.Block{{Type: ir.BlockText, Text: "ok"}}, Usage: cacheDetailsUsage()}
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses} {
		if notes := responseLossyNotes(t, name, resp); !hasCacheDetailsNote(notes) {
			t.Errorf("%s 应报 TTL 明细丢弃：%v", name, notes)
		}
	}
	if notes := responseLossyNotes(t, codec.ProtocolAnthropic, resp); hasCacheDetailsNote(notes) {
		t.Errorf("anthropic 自家接得住，误报：%v", notes)
	}
	// 明细未知：没东西可丢，全静默。
	resp.Usage = ir.Usage{InputTokens: 100, OutputTokens: 5, CacheWriteTokens: 30}
	for _, name := range []string{codec.ProtocolAnthropic, codec.ProtocolChatCompletions, codec.ProtocolResponses} {
		if notes := responseLossyNotes(t, name, resp); hasCacheDetailsNote(notes) {
			t.Errorf("%s 明细未知误报：%v", name, notes)
		}
	}
}

// encodeAll 把事件逐个喂给编码器并拼出全部输出帧。
func encodeAll(t *testing.T, enc codec.StreamEncoder, events []ir.Event) string {
	t.Helper()
	var b strings.Builder
	for _, ev := range events {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("encode %s: %v", ev.Type, err)
		}
		for _, f := range frames {
			b.Write(f)
		}
	}
	for _, f := range enc.Finish() {
		b.Write(f)
	}
	return b.String()
}
