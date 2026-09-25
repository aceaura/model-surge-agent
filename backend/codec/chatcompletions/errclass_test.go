package chatcompletions

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 流内错误帧没有 HTTP 状态码，只能靠 error.type/code 归类。全归 upstream 会让
// 限流被当成目标故障去累计失败并冷却，而它本该只是等一等再发。
func TestStreamErrorClassifiesByCode(t *testing.T) {
	cases := []struct {
		code string
		want ir.ErrorKind
	}{
		{"rate_limit_exceeded", ir.ErrRateLimit},
		{"insufficient_quota", ir.ErrRateLimit},
		{"invalid_api_key", ir.ErrAuth},
		{"model_not_found", ir.ErrNotFound},
		{"context_length_exceeded", ir.ErrContextExceeded},
		{"invalid_request_error", ir.ErrInvalidRequest},
		{"server_error", ir.ErrUpstream},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			dec := newStreamDecoder()
			events, err := dec.Feed("",
				`{"error":{"type":"`+tc.code+`","message":"boom"}}`)
			if err != nil {
				t.Fatalf("Feed: %v", err)
			}
			if len(events) != 1 || events[0].Type != ir.EvError || events[0].Err == nil {
				t.Fatalf("events = %#v, want one error event", events)
			}
			if got := events[0].Err.Kind; got != tc.want {
				t.Errorf("kind = %q, want %q", got, tc.want)
			}
		})
	}
}

// HTTP 路径与流内路径必须给出同一个分类，否则同一个上游故障会因为
// 走了哪条路而被记成两种结果。
func TestStreamErrorMatchesTheHTTPPath(t *testing.T) {
	body := `{"error":{"type":"rate_limit_exceeded","message":"slow down"}}`
	dec := newStreamDecoder()
	events, err := dec.Feed("", body)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	viaHTTP := DecodeError(429, nil, []byte(body))
	if events[0].Err.Kind != viaHTTP.Kind {
		t.Errorf("stream kind = %q, http kind = %q", events[0].Err.Kind, viaHTTP.Kind)
	}
}

// 本协议的兼容实现最多，错误体形状五花八门：仍要挖出消息，
// 不能只丢一段原始 body 给运维。
func TestDecodeErrorExtractsFromForeignShapes(t *testing.T) {
	got := DecodeError(503, nil, []byte(`{"detail":"gateway exploded"}`))
	if got.Message != "gateway exploded" {
		t.Errorf("message = %q, want the message dug out of the foreign shape", got.Message)
	}
}

// 上游把下游的整个错误体字符串化塞进 message：真消息在里面。
func TestDecodeErrorUnwrapsJSONStuffedIntoMessage(t *testing.T) {
	body := `{"error":{"type":"invalid_request_error",` +
		`"message":"{\"error\":{\"message\":\"prompt is too long\"}}"}}`
	got := DecodeError(400, nil, []byte(body))
	if got.Message != "prompt is too long" {
		t.Errorf("message = %q, the real message is nested inside", got.Message)
	}
	if got.Kind != ir.ErrContextExceeded {
		t.Errorf("kind = %q, want context_exceeded", got.Kind)
	}
}

// param 要一路带到 ir.Error：客户端靠它知道改哪个字段。
// 兼容层代理常在本协议的端点上回 openai 形状的错误体，所以即便本协议
// 自家的错误结构没有 param 位，也要能从原始字节里挖出来。
func TestDecodeErrorCarriesParam(t *testing.T) {
	got := DecodeError(400, nil, []byte(`{"error":{"message":"bad value","param":"max_tokens"}}`))
	if got.Param != "max_tokens" {
		t.Errorf("param = %q, want max_tokens", got.Param)
	}
}

func TestDecodeErrorLeavesParamEmptyWhenAbsent(t *testing.T) {
	got := DecodeError(400, nil, []byte(`{"error":{"message":"bad value"}}`))
	if got.Param != "" {
		t.Errorf("param = %q, 上游没给就该缺席而不是空串以外的值", got.Param)
	}
}

// param 要出现在本协议的错误信封里：它是客户端定位问题字段的唯一线索。
func TestRenderErrorCarriesParam(t *testing.T) {
	e := ir.NewError(ir.ErrInvalidRequest, 400, "", "bad value")
	e.Param = "max_tokens"
	_, body := RenderError(e)
	if !strings.Contains(string(body), `"param":"max_tokens"`) {
		t.Errorf("body = %s, 应含 param", body)
	}
}

// 流内错误同样要带 param：committed 之后状态码改不了，流内那一帧是唯一出口。
func TestRenderStreamErrorCarriesParam(t *testing.T) {
	e := ir.NewError(ir.ErrInvalidRequest, 400, "", "bad value")
	e.Param = "max_tokens"
	var joined string
	for _, f := range RenderStreamError(e) {
		joined += string(f)
	}
	if !strings.Contains(joined, "max_tokens") {
		t.Errorf("frames = %s, 应含 param", joined)
	}
}

// 无 param 时字段要缺席，而不是渲染成空串：客户端会把空串当成一个真字段名。
func TestErrorEnvelopeOmitsEmptyParam(t *testing.T) {
	_, body := RenderError(ir.NewError(ir.ErrInvalidRequest, 400, "", "bad"))
	if strings.Contains(string(body), "param") {
		t.Errorf("body = %s, 无 param 时该字段应缺席", body)
	}
}

// 本协议的流式形态是扁平的 choices[].delta，没有块生命周期，也就没有可
// 悬空的状态机——无需闭合帧。但 [DONE] 必须发：客户端靠它判定流结束。
func TestErrorStreamCarriesDoneWithoutFinishReason(t *testing.T) {
	e := newStreamEncoder()
	var joined string
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "chatcmpl_1"},
		{Type: ir.EvTextDelta, Index: 0, Text: "half"},
		{Type: ir.EvError, Err: ir.NewError(ir.ErrUpstream, 500, "", "boom")},
	} {
		frames, err := e.Encode(ev)
		if err != nil {
			t.Fatalf("encode %s: %v", ev.Type, err)
		}
		for _, f := range frames {
			joined += string(f)
		}
	}
	for _, f := range e.Finish() {
		joined += string(f)
	}
	if !strings.Contains(joined, "[DONE]") {
		t.Errorf("缺 [DONE]，客户端会挂到超时: %s", joined)
	}
	if strings.Contains(joined, "finish_reason") {
		t.Errorf("错误收尾不得带 finish_reason: %s", joined)
	}
	if strings.Count(joined, "[DONE]") != 1 {
		t.Errorf("[DONE] 只该出现一次: %s", joined)
	}
}

// message 写成数字（部分代理的形态）时不得连累解得好的 code：消息回落
// 原文/状态码描述，归因靠的错误码留住。修复前整个 error 对象被丢弃，
// Code 恒空、消息是一整段转义原文。
func TestDecodeErrorKeepsCodeWhenMessageIsNumeric(t *testing.T) {
	got := DecodeError(400, nil, []byte(`{"error":{"code":"context_length_exceeded","message":400}}`))
	if got.Code != "context_length_exceeded" {
		t.Errorf("code = %q, want context_length_exceeded", got.Code)
	}
	if got.Message == "" {
		t.Error("message 不得为空：流水里查不出任何东西")
	}
}
