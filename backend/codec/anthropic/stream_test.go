package anthropic

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// fullStream 覆盖一次带推理、文本与工具调用的完整回合。
const fullStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-5","role":"assistant","usage":{"input_tokens":120,"cache_read_input_tokens":80}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"let me"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-1"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: ping
data: {"type":"ping"}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"searching"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"tu_1","name":"grep","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"pattern\""}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":":\"x\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":64}}

event: message_stop
data: {"type":"message_stop"}

`

// feed 把 SSE 文本喂进解码器，返回全部 IR 事件。
func feed(t *testing.T, raw string) []ir.Event {
	t.Helper()
	dec := newStreamDecoder()
	scanner := codec.NewFrameScanner(strings.NewReader(raw))
	var events []ir.Event
	for scanner.Scan() {
		f := scanner.Frame()
		out, err := dec.Feed(f.Event, f.Data)
		if err != nil {
			t.Fatalf("Feed(%q): %v", f.Event, err)
		}
		events = append(events, out...)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return append(events, dec.Finish()...)
}

func TestDecodeStreamProducesFullEventVocabulary(t *testing.T) {
	events := feed(t, fullStream)

	var got []ir.EventType
	for _, e := range events {
		got = append(got, e.Type)
	}
	want := []ir.EventType{
		ir.EvMessageStart,
		ir.EvBlockStart, ir.EvThinkingDelta, ir.EvSigDelta, ir.EvBlockStop,
		ir.EvPing,
		ir.EvBlockStart, ir.EvTextDelta, ir.EvBlockStop,
		ir.EvBlockStart, ir.EvToolInput, ir.EvToolInput, ir.EvBlockStop,
		ir.EvMessageDelta, ir.EvMessageStop,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("event sequence =\n%v\nwant\n%v", got, want)
	}
	if events[0].MessageID != "msg_1" || events[0].Model != "claude-opus-5" {
		t.Errorf("message_start lost identity: %+v", events[0])
	}
	if u := events[0].Usage; u == nil || u.InputTokens != 120 || u.CacheReadTokens != 80 {
		t.Errorf("message_start usage = %+v", u)
	}
}

func TestAggregateStreamMatchesSemantics(t *testing.T) {
	agg := &ir.Aggregator{}
	for _, e := range feed(t, fullStream) {
		agg.Add(e)
	}
	resp := agg.Response()

	if resp.ID != "msg_1" || resp.Model != "claude-opus-5" {
		t.Errorf("identity lost: %+v", resp)
	}
	if resp.StopReason != ir.StopToolUse {
		t.Errorf("stop_reason = %q", resp.StopReason)
	}
	if resp.Usage.InputTokens != 120 || resp.Usage.OutputTokens != 64 || resp.Usage.CacheReadTokens != 80 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if len(resp.Content) != 3 {
		t.Fatalf("content = %d blocks: %+v", len(resp.Content), resp.Content)
	}
	th := resp.Content[0].Thinking
	if th == nil || th.Text != "let me" || th.Signature != "sig-1" {
		t.Errorf("thinking block = %+v", th)
	}
	if resp.Content[1].Text != "searching" {
		t.Errorf("text block = %q", resp.Content[1].Text)
	}
	tu := resp.Content[2].ToolUse
	if tu == nil || tu.ID != "tu_1" || tu.Input != `{"pattern":"x"}` {
		t.Errorf("tool_use block = %+v", tu)
	}
}

// 流往返：解码再编码，重新解一次应得到同一串事件。
func TestStreamRoundTripPreservesEvents(t *testing.T) {
	first := feed(t, fullStream)

	enc := newStreamEncoder()
	var wire strings.Builder
	for _, e := range first {
		frames, err := enc.Encode(e)
		if err != nil {
			t.Fatalf("Encode(%v): %v", e.Type, err)
		}
		for _, f := range frames {
			wire.Write(f)
		}
	}
	for _, f := range enc.Finish() {
		wire.Write(f)
	}

	second := feed(t, wire.String())
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("stream round trip drifted:\n first = %v\nsecond = %v", first, second)
	}
}

// message_start 缺失时必须补发：客户端 SDK 见到孤立的 delta 会报错。
func TestEncoderSynthesizesMissingMessageStart(t *testing.T) {
	enc := newStreamEncoder()
	frames, err := enc.Encode(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "hi"})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	out := join(frames)
	if !strings.Contains(out, "event: message_start") {
		t.Errorf("message_start not synthesized: %s", out)
	}
	if !strings.Contains(out, "event: content_block_start") {
		t.Errorf("content_block_start not synthesized: %s", out)
	}
	if strings.Index(out, "message_start") > strings.Index(out, "content_block_start") {
		t.Errorf("message_start must come first: %s", out)
	}
}

// 上游中途断流：Finish 必须闭合块并补终止帧，否则客户端一直等。
func TestFinishClosesTruncatedStream(t *testing.T) {
	enc := newStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "msg_1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Encode(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "partial"}); err != nil {
		t.Fatal(err)
	}
	out := join(enc.Finish())
	for _, want := range []string{"content_block_stop", "message_delta", "message_stop"} {
		if !strings.Contains(out, want) {
			t.Errorf("Finish must emit %s: %s", want, out)
		}
	}
	if second := enc.Finish(); second != nil {
		t.Errorf("Finish must be idempotent, got %s", join(second))
	}
}

// 上游没发 message_stop 时解码器要补一个，下游才知道流结束了。
func TestDecoderFinishSynthesizesStop(t *testing.T) {
	dec := newStreamDecoder()
	if _, err := dec.Feed(evMessageStart, `{"type":"message_start","message":{"id":"m"}}`); err != nil {
		t.Fatal(err)
	}
	got := dec.Finish()
	if len(got) != 1 || got[0].Type != ir.EvMessageStop {
		t.Fatalf("Finish = %+v, want a synthesized message_stop", got)
	}
	if extra := dec.Finish(); extra != nil {
		t.Errorf("Finish must be idempotent, got %+v", extra)
	}
}

// 上游新增的 delta 类型不该让整个流失败。
func TestDecoderSkipsUnknownDeltaType(t *testing.T) {
	dec := newStreamDecoder()
	got, err := dec.Feed(evContentBlockDelta,
		`{"type":"content_block_delta","index":0,"delta":{"type":"some_future_delta"}}`)
	if err != nil {
		t.Fatalf("unknown delta must not fail the stream: %v", err)
	}
	if got != nil {
		t.Errorf("want no events, got %+v", got)
	}
}

func TestDecodeResponse(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5",
      "content":[{"type":"text","text":"done"},{"type":"redacted_thinking","data":"opaque"}],
      "stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`
	resp, err := DecodeResponse([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	// D2：同族响应里的涂抹块原样保留，密文不丢——它是这段被涂抹推理唯一的
	// 无损归宿，下一轮同族回传还要靠它。
	if len(resp.Content) != 2 || resp.Content[0].Text != "done" {
		t.Fatalf("want text + redacted_thinking preserved, got %+v", resp.Content)
	}
	rt := resp.Content[1]
	if rt.Type != ir.BlockThinking || rt.Thinking == nil || !rt.Thinking.Redacted || rt.Thinking.RedactedData != "opaque" {
		t.Errorf("redacted_thinking must round-trip with its ciphertext intact: %+v", rt)
	}
	if resp.StopReason != ir.StopEndTurn || resp.Usage.OutputTokens != 5 {
		t.Errorf("resp = %+v", resp)
	}

	wire, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	again, err := DecodeResponse(wire)
	if err != nil {
		t.Fatalf("DecodeResponse(round 2): %v", err)
	}
	if !reflect.DeepEqual(resp, again) {
		t.Fatalf("response round trip drifted:\n%+v\n%+v", resp, again)
	}
}

// 上下文超限与普通参数错误都是 400，但前者不该记作目标失败，
// 所以必须靠消息内容分开。
func TestDecodeErrorSplitsContextOverflowFrom400(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   ir.ErrorKind
		retry  bool
	}{
		{"context overflow", 400,
			`{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 300000 tokens"}}`,
			ir.ErrContextExceeded, false},
		{"ordinary bad request", 400,
			`{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens must be positive"}}`,
			ir.ErrInvalidRequest, false},
		{"payload too large", 413, `{}`, ir.ErrContextExceeded, false},
		{"rate limited", 429,
			`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`,
			ir.ErrRateLimit, true},
		{"auth", 401, `{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`,
			ir.ErrAuth, false},
		{"not found", 404, `{"type":"error","error":{"type":"not_found_error","message":"no model"}}`,
			ir.ErrNotFound, false},
		{"upstream", 503, `<html>gateway down</html>`, ir.ErrUpstream, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DecodeError(c.status, nil, []byte(c.body))
			if got.Kind != c.want {
				t.Errorf("kind = %q, want %q", got.Kind, c.want)
			}
			if got.Retryable != c.retry {
				t.Errorf("retryable = %v, want %v", got.Retryable, c.retry)
			}
			if got.StatusCode != c.status {
				t.Errorf("status = %d", got.StatusCode)
			}
		})
	}
}

// 非 JSON 响应体（网关 HTML 页）要保留片段供排查，但不能无界。
func TestDecodeErrorKeepsBoundedRawBody(t *testing.T) {
	got := DecodeError(502, nil, []byte(strings.Repeat("x", 4096)))
	if len(got.Message) > 600 {
		t.Fatalf("message must be truncated, got %d bytes", len(got.Message))
	}
	if !strings.Contains(got.Message, "502") {
		t.Errorf("message should mention the status: %q", got.Message)
	}
}

func TestRenderError(t *testing.T) {
	status, body := RenderError(ir.NewError(ir.ErrRateLimit, 0, "", "slow down"))
	if status != 429 {
		t.Errorf("status = %d", status)
	}
	var env wireErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if env.Type != "error" || env.Error.Type != "rate_limit_error" || env.Error.Message != "slow down" {
		t.Errorf("envelope = %+v", env)
	}
}

// committed 之后 HTTP 200 已写出，错误只能以流内 error 帧表达。
func TestRenderStreamError(t *testing.T) {
	frames := RenderStreamError(ir.NewError(ir.ErrUpstream, 500, "", "connection reset"))
	out := join(frames)
	if !strings.HasPrefix(out, "event: error\n") {
		t.Fatalf("want an error frame, got %q", out)
	}
	if !strings.Contains(out, "connection reset") {
		t.Errorf("frame should carry the message: %q", out)
	}
}

func join(frames [][]byte) string {
	var b strings.Builder
	for _, f := range frames {
		b.Write(f)
	}
	return b.String()
}
