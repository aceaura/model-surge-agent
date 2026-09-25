package responses

import (
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 官方裸 error 帧的错误体在顶层 error 键下（{"type":"error","error":{...}}），
// 不是平铺三键。只读平铺会把 type/code/message 全部静默丢掉，
// 客户端只看到一个空错误。
func TestStreamErrorReadsNestedErrorBody(t *testing.T) {
	dec := newStreamDecoder()
	events, err := dec.Feed("error",
		`{"type":"error","error":{"type":"server_error","code":"server_error","message":"boom nested"}}`)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(events) != 1 || events[0].Type != ir.EvError || events[0].Err == nil {
		t.Fatalf("events = %#v, want one error event", events)
	}
	got := events[0].Err
	if got.Message != "boom nested" {
		t.Errorf("message = %q, want the nested body's message", got.Message)
	}
	if got.Code != "server_error" {
		t.Errorf("code = %q, want server_error", got.Code)
	}
	if got.Kind != ir.ErrUpstream {
		t.Errorf("kind = %q, want upstream", got.Kind)
	}
}

// 平铺三键的网关形态仍要能归类（与 errclass 测试同一口径），
// 且嵌套层缺席时不得被兜底消息覆盖掉真实内容。
func TestStreamErrorFlatFormStillWorks(t *testing.T) {
	dec := newStreamDecoder()
	events, err := dec.Feed("error",
		`{"type":"error","code":"rate_limit_exceeded","message":"slow down"}`)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if events[0].Err.Kind != ir.ErrRateLimit || events[0].Err.Message != "slow down" {
		t.Errorf("err = %#v, want rate_limit classification preserved", events[0].Err)
	}
}

// 三层全空时给兜底消息，客户端不能拿到一个空错误。
func TestStreamErrorFallsBackToPlaceholderMessage(t *testing.T) {
	dec := newStreamDecoder()
	events, err := dec.Feed("error", `{"type":"error"}`)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if events[0].Err.Message != "upstream stream error" {
		t.Errorf("message = %q, want fallback", events[0].Err.Message)
	}
}

// 错误是终止性的：裸 error 之后上游再发 completed，不得产生一份
// StopEndTurn 的正常收尾把失败伪装成成功。
func TestBareErrorSuppressesLaterCompleted(t *testing.T) {
	dec := newStreamDecoder()
	if _, err := dec.Feed("error",
		`{"type":"error","error":{"code":"server_error","message":"boom"}}`); err != nil {
		t.Fatalf("Feed error: %v", err)
	}
	events, err := dec.Feed("response.completed",
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	if err != nil {
		t.Fatalf("Feed completed: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("completed 之后不得再有事件: %#v", events)
	}
}

// 上游的真实序列是 error → response.failed（sub2api 夹具可证）：
// 错误详情只在裸 error 帧里，failed 帧不得再补一份错误，
// 否则客户端看到两次报错。
func TestErrorThenFailedEmitsExactlyOneError(t *testing.T) {
	dec := newStreamDecoder()
	first, err := dec.Feed("error",
		`{"type":"error","error":{"code":"server_error","message":"boom"}}`)
	if err != nil {
		t.Fatalf("Feed error: %v", err)
	}
	if len(first) != 1 || first[0].Type != ir.EvError {
		t.Fatalf("first = %#v, want one error", first)
	}
	second, err := dec.Feed("response.failed",
		`{"type":"response.failed","response":{"id":"r1","status":"failed","error":{"code":"server_error","message":"boom"}}}`)
	if err != nil {
		t.Fatalf("Feed failed: %v", err)
	}
	for _, ev := range second {
		if ev.Type == ir.EvError {
			t.Fatalf("failed 帧重复交错误: %#v", second)
		}
	}
}

// failed 帧不带任何错误体时也要交一个兜底错误再收流，
// 不能让失败流只有 delta+stop（会被下游当成正常结束）。
func TestFailedWithoutErrorBodyStillEmitsError(t *testing.T) {
	dec := newStreamDecoder()
	events, err := dec.Feed("response.failed",
		`{"type":"response.failed","response":{"id":"r1","status":"failed"}}`)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	var sawErr bool
	for _, ev := range events {
		if ev.Type == ir.EvError {
			sawErr = true
			if ev.Err == nil || ev.Err.Message != "upstream response failed" {
				t.Errorf("err = %#v, want fallback message", ev.Err)
			}
		}
	}
	if !sawErr {
		t.Fatalf("failed 帧缺错误事件: %#v", events)
	}
}

// failed 帧带 response.error：走第二层回落，详情原样交出。
func TestFailedCarriesResponseError(t *testing.T) {
	dec := newStreamDecoder()
	events, err := dec.Feed("response.failed",
		`{"type":"response.failed","response":{"id":"r1","status":"failed",`+
			`"error":{"code":"rate_limit_exceeded","message":"slow down"}}}`)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	var found *ir.Error
	for _, ev := range events {
		if ev.Type == ir.EvError {
			found = ev.Err
		}
	}
	if found == nil {
		t.Fatalf("缺错误事件: %#v", events)
	}
	if found.Message != "slow down" || found.Kind != ir.ErrRateLimit {
		t.Errorf("err = %#v, want the response.error detail", found)
	}
}

// 裸 error 已经把流收成终止态：之后 Finish() 不得再补正常收尾。
func TestFinishAfterBareErrorIsSilent(t *testing.T) {
	dec := newStreamDecoder()
	if _, err := dec.Feed("error",
		`{"type":"error","error":{"code":"server_error","message":"boom"}}`); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if events := dec.Finish(); len(events) != 0 {
		t.Fatalf("Finish 应静默: %#v", events)
	}
}

// 迟到的错误详情（completed 之后才来的裸 error 帧）仍要交出去：
// 上游实现不一，错误帧可能排在收尾帧后面。
func TestLateBareErrorAfterCompletedIsDelivered(t *testing.T) {
	dec := newStreamDecoder()
	if _, err := dec.Feed("response.completed",
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`); err != nil {
		t.Fatalf("Feed completed: %v", err)
	}
	events, err := dec.Feed("error",
		`{"type":"error","error":{"code":"server_error","message":"late boom"}}`)
	if err != nil {
		t.Fatalf("Feed error: %v", err)
	}
	if len(events) != 1 || events[0].Type != ir.EvError || events[0].Err.Message != "late boom" {
		t.Fatalf("events = %#v, want the late error delivered", events)
	}
}
