package responses

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 流内错误帧没有 HTTP 状态码，只能靠 code 归类。全归 upstream 会让限流被
// 当成目标故障去累计失败并冷却，而它本该只是等一等再发。
func TestStreamErrorClassifiesByCode(t *testing.T) {
	cases := []struct {
		code string
		want ir.ErrorKind
	}{
		{"rate_limit_exceeded", ir.ErrRateLimit},
		{"invalid_api_key", ir.ErrAuth},
		{"model_not_found", ir.ErrNotFound},
		{"context_length_exceeded", ir.ErrContextExceeded},
		{"invalid_request_error", ir.ErrInvalidRequest},
		{"server_error", ir.ErrUpstream},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			dec := newStreamDecoder()
			events, err := dec.Feed("error",
				`{"type":"error","code":"`+tc.code+`","message":"boom"}`)
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
	dec := newStreamDecoder()
	events, err := dec.Feed("error",
		`{"type":"error","code":"rate_limit_exceeded","message":"slow down"}`)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	viaHTTP := DecodeError(429,
		[]byte(`{"error":{"code":"rate_limit_exceeded","message":"slow down"}}`))
	if events[0].Err.Kind != viaHTTP.Kind {
		t.Errorf("stream kind = %q, http kind = %q", events[0].Err.Kind, viaHTTP.Kind)
	}
}

// 上游没按本协议的两种规范形状回：仍要挖出消息，不能只丢一段原始 body。
func TestDecodeErrorExtractsFromForeignShapes(t *testing.T) {
	got := DecodeError(503, []byte(`{"err":"gateway exploded"}`))
	if got.Message != "gateway exploded" {
		t.Errorf("message = %q, want the message dug out of the foreign shape", got.Message)
	}
}

// 上游把下游的整个错误体字符串化塞进 message：真消息在里面。
func TestDecodeErrorUnwrapsJSONStuffedIntoMessage(t *testing.T) {
	body := `{"error":{"code":"invalid_request_error",` +
		`"message":"{\"error\":{\"message\":\"prompt is too long\"}}"}}`
	got := DecodeError(400, []byte(body))
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
	got := DecodeError(400, []byte(`{"error":{"message":"bad value","param":"max_tokens"}}`))
	if got.Param != "max_tokens" {
		t.Errorf("param = %q, want max_tokens", got.Param)
	}
}

func TestDecodeErrorLeavesParamEmptyWhenAbsent(t *testing.T) {
	got := DecodeError(400, []byte(`{"error":{"message":"bad value"}}`))
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

// 错误收尾三件事：闭合条目（标 incomplete）、发 error、补 response.failed。
// 不发 response.completed——那会把失败说成成功。
func TestErrorClosesItemsThenFails(t *testing.T) {
	e := newStreamEncoder()
	var joined string
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "resp_1"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
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
	done := strings.Index(joined, "output_item.done")
	boom := strings.Index(joined, "boom")
	failed := strings.Index(joined, "response.failed")
	if done < 0 {
		t.Fatalf("缺条目闭合帧: %s", joined)
	}
	if done > boom {
		t.Errorf("闭合帧必须排在错误帧之前: %s", joined)
	}
	if failed < 0 {
		t.Fatalf("缺 response.failed，客户端会一直等终态: %s", joined)
	}
	if strings.Contains(joined, "response.completed") {
		t.Errorf("错误收尾不得宣告 completed: %s", joined)
	}
	// 截断的条目不能标成 completed：那一刻入参可能只有半截 JSON。
	if !strings.Contains(joined, `"status":"incomplete"`) {
		t.Errorf("错误收尾闭合的条目应标 incomplete: %s", joined)
	}
}
