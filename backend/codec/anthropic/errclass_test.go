package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 流内错误帧没有 HTTP 状态码，只能靠 error.type 归类。全归 upstream 会让
// 限流被当成目标故障去累计失败并冷却，而它本该只是等一等再发。
func TestStreamErrorClassifiesByType(t *testing.T) {
	cases := []struct {
		typ  string
		want ir.ErrorKind
	}{
		{"rate_limit_error", ir.ErrRateLimit},
		{"authentication_error", ir.ErrAuth},
		{"not_found_error", ir.ErrNotFound},
		{"invalid_request_error", ir.ErrInvalidRequest},
		{"overloaded_error", ir.ErrUpstream},
		{"api_error", ir.ErrUpstream},
	}
	for _, tc := range cases {
		t.Run(tc.typ, func(t *testing.T) {
			dec := newStreamDecoder()
			events, err := dec.Feed("error",
				`{"type":"error","error":{"type":"`+tc.typ+`","message":"boom"}}`)
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
	body := `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`
	dec := newStreamDecoder()
	events, err := dec.Feed("error", body)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	viaHTTP := DecodeError(429, []byte(body))
	if events[0].Err.Kind != viaHTTP.Kind {
		t.Errorf("stream kind = %q, http kind = %q", events[0].Err.Kind, viaHTTP.Kind)
	}
}

// 上游把下游的整个错误体字符串化塞进 message：真消息在里面，
// 上下文超限的判定要看消息文本，读到一串转义引号就判不出来了。
func TestDecodeErrorUnwrapsJSONStuffedIntoMessage(t *testing.T) {
	body := `{"type":"error","error":{"type":"invalid_request_error",` +
		`"message":"{\"error\":{\"message\":\"prompt is too long\"}}"}}`
	got := DecodeError(400, []byte(body))
	if got.Message != "prompt is too long" {
		t.Errorf("message = %q, the real message is nested inside", got.Message)
	}
	if got.Kind != ir.ErrContextExceeded {
		t.Errorf("kind = %q, want context_exceeded", got.Kind)
	}
}

// 上游没按本协议的错误结构回：仍要挖出消息，不能只丢一段原始 body。
func TestDecodeErrorExtractsFromForeignShapes(t *testing.T) {
	got := DecodeError(503, []byte(`{"msg":"gateway exploded"}`))
	if got.Message != "gateway exploded" {
		t.Errorf("message = %q, want the message dug out of the foreign shape", got.Message)
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

// 本协议的错误信封没有 param 位，丢掉时必须留下说明——这条丢弃是真的丢信息，
// 且只在上游给了 param 时才出现，所以它指得出是哪条路由削弱了错误诊断。
func TestRenderErrorReportsDroppedParam(t *testing.T) {
	e := ir.NewError(ir.ErrInvalidRequest, 400, "", "bad value")
	e.Param = "max_tokens"
	status, body, notes := RenderErrorLossy(e)
	if len(notes) == 0 {
		t.Fatal("丢掉 param 却没有 lossy 说明")
	}
	if !strings.Contains(notes[0], "max_tokens") {
		t.Errorf("说明 = %q, 应指出被丢掉的字段名", notes[0])
	}
	if strings.Contains(string(body), "param") {
		t.Errorf("body = %s, 本协议不该出现 param 字段", body)
	}
	// 两条路径必须逐字节一致，否则排查动作本身会改变客户端看到的内容。
	plainStatus, plain := RenderError(e)
	if plainStatus != status || string(plain) != string(body) {
		t.Errorf("RenderError 与 RenderErrorLossy 字节不一致")
	}
}

// 上游没给 param 时不该恒定冒出一条说明：那样它就再也指不出任何东西。
func TestRenderErrorStaysSilentWithoutParam(t *testing.T) {
	_, _, notes := RenderErrorLossy(ir.NewError(ir.ErrInvalidRequest, 400, "", "bad"))
	if len(notes) != 0 {
		t.Errorf("notes = %v, 无 param 时不该有说明", notes)
	}
}

// 错误收尾要先闭合已开的块，再发错误帧，且不补正常终止帧。
func TestErrorClosesOpenBlocksBeforeTheErrorFrame(t *testing.T) {
	e := newStreamEncoder()
	var joined string
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "msg_1"},
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
	stop := strings.Index(joined, "content_block_stop")
	boom := strings.Index(joined, "boom")
	if stop < 0 {
		t.Fatalf("缺块闭合帧: %s", joined)
	}
	if stop > boom {
		t.Errorf("闭合帧必须排在错误帧之前: %s", joined)
	}
	if strings.Contains(joined, "message_stop") {
		t.Errorf("错误收尾不得含正常终止帧: %s", joined)
	}
}
