package chatcompletions

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 本文件守 stream_options.include_usage 的三态：没给 / 明确 true / 明确 false。
// 判据 1–9。

func decode(t *testing.T, body string) *ir.Request {
	t.Helper()
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	return req
}

const streamBody = `{"model":"m","stream":true,
  "messages":[{"role":"user","content":"hi"}]%s}`

// 判据 1：没给 stream_options 时是「没提」，不是 false。
func TestIncludeUsageUnsetIsNil(t *testing.T) {
	req := decode(t, `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if req.IncludeUsage != nil {
		t.Errorf("IncludeUsage = %v，want nil（没提）", *req.IncludeUsage)
	}
}

// 判据 2：明确 true。
func TestIncludeUsageExplicitTrue(t *testing.T) {
	req := decode(t, `{"model":"m","stream":true,"stream_options":{"include_usage":true},
	  "messages":[{"role":"user","content":"hi"}]}`)
	if req.IncludeUsage == nil || !*req.IncludeUsage {
		t.Fatalf("IncludeUsage = %v，want true", req.IncludeUsage)
	}
}

// 判据 3：给了 stream_options 但没写 include_usage 是明确的 false——
// JSON 的零值就是这个字段的语义，与压根没给 stream_options 不同。
func TestIncludeUsageEmptyOptionsIsExplicitFalse(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","stream":true,"stream_options":{},"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","stream":true,"stream_options":{"include_usage":false},
		  "messages":[{"role":"user","content":"hi"}]}`,
	} {
		req := decode(t, body)
		if req.IncludeUsage == nil {
			t.Fatalf("body %s → nil，want 明确 false", body)
		}
		if *req.IncludeUsage {
			t.Errorf("body %s → true", body)
		}
	}
}

// finishFrames 把一条最短的完整流编出来，返回收尾帧。
func finishFrames(t *testing.T, req *ir.Request) string {
	t.Helper()
	enc := inboundCodec{}.NewStreamEncoder(req)
	var sb strings.Builder
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 0, Text: "hi"},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn,
			Usage: &ir.Usage{InputTokens: 10, OutputTokens: 3}},
		{Type: ir.EvMessageStop},
	} {
		out, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("Encode(%v): %v", ev.Type, err)
		}
		for _, f := range out {
			sb.Write(f)
		}
	}
	for _, f := range enc.Finish() {
		sb.Write(f)
	}
	return sb.String()
}

// usageFrames 数出「choices 为空数组且带 usage」的那种帧。
// 按结构数而不是按 `"usage"` 子串数：正文帧里也可能出现这个词，
// 而本判据要的恰恰是那一帧单独的 usage。
func usageFrames(t *testing.T, stream string) int {
	t.Helper()
	n := 0
	for _, line := range strings.Split(stream, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || data == doneSentinel {
			continue
		}
		var w struct {
			Choices []json.RawMessage `json:"choices"`
			Usage   json.RawMessage   `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &w); err != nil {
			t.Fatalf("帧不是合法 JSON：%s", data)
		}
		if len(w.Choices) == 0 && len(w.Usage) > 0 {
			n++
		}
	}
	return n
}

// 判据 4：没提时照发（既有行为，不能因为这轮改动而变）。
func TestUnsetStillSendsTheUsageFrame(t *testing.T) {
	out := finishFrames(t, decode(t,
		`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if got := usageFrames(t, out); got != 1 {
		t.Errorf("usage 帧 %d 个，want 1：\n%s", got, out)
	}
}

// 判据 5：明确 true 照发。
func TestExplicitTrueSendsTheUsageFrame(t *testing.T) {
	out := finishFrames(t, decode(t,
		`{"model":"m","stream":true,"stream_options":{"include_usage":true},
		  "messages":[{"role":"user","content":"hi"}]}`))
	if got := usageFrames(t, out); got != 1 {
		t.Errorf("usage 帧 %d 个，want 1：\n%s", got, out)
	}
}

// 判据 6：明确 false 不发那一帧。
func TestExplicitFalseSuppressesTheUsageFrame(t *testing.T) {
	out := finishFrames(t, decode(t,
		`{"model":"m","stream":true,"stream_options":{"include_usage":false},
		  "messages":[{"role":"user","content":"hi"}]}`))
	if got := usageFrames(t, out); got != 0 {
		t.Errorf("usage 帧 %d 个，want 0——客户端明确说了不要：\n%s", got, out)
	}
}

// 判据 7：只压 usage 那一帧，终止形状照旧。
//
// 压掉 finish_reason 的空 delta 或 [DONE] 会让客户端等一个永不到来的结束，
// 那是比多一帧 usage 严重得多的破坏。
func TestSuppressionKeepsTheTerminationShape(t *testing.T) {
	out := finishFrames(t, decode(t,
		`{"model":"m","stream":true,"stream_options":{"include_usage":false},
		  "messages":[{"role":"user","content":"hi"}]}`))
	if !strings.Contains(out, doneSentinel) {
		t.Errorf("缺 [DONE]：\n%s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Errorf("缺 finish_reason 帧：\n%s", out)
	}
}

// 判据 8：照客户端要求执行不算有损。
//
// 记成有损会让运维在流水里看到一条永远解决不了的诊断：
// 「丢了 usage」而客户端本来就不要它。
func TestSuppressionIsNotLossy(t *testing.T) {
	enc := inboundCodec{}.NewStreamEncoder(decode(t,
		`{"model":"m","stream":true,"stream_options":{"include_usage":false},
		  "messages":[{"role":"user","content":"hi"}]}`))
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageStart}); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	enc.Finish()
	n, ok := enc.(codec.StreamNotes)
	if !ok {
		t.Fatal("编码器没实现 StreamNotes")
	}
	if notes := n.Notes(); len(notes) != 0 {
		t.Errorf("notes = %#v，want 空", notes)
	}
}

// 判据 9：对上游一律要 usage，与客户端的表态无关——记账要用它。
//
// 把客户端的 false 透传给上游会让流水里的 token 数恒为估算值，
// 而计费对账要的是上游报的那个数。
func TestOutboundAlwaysAsksUpstreamForUsage(t *testing.T) {
	req := decode(t, `{"model":"m","stream":true,"stream_options":{"include_usage":false},
	  "messages":[{"role":"user","content":"hi"}]}`)
	body, err := outboundCodec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	var w struct {
		StreamOptions *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if w.StreamOptions == nil || !w.StreamOptions.IncludeUsage {
		t.Errorf("出站 stream_options = %+v，want include_usage:true", w.StreamOptions)
	}
}
