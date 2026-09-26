package responses

import (
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// P3 的端到端效果：判据表（errkind.go）改了不算数，只有流内错误帧真的走到
// streamError → convertError → KindFor 并产出不可重试的 content_filter 才算数。
//
// 裸 error 帧的错误体在顶层（{"type":"error","error":{...}}），这是官方形态，
// 也是 cyber_policy 拦截实际到达的形态。
func TestStreamErrorCyberPolicyIsNonRetryable(t *testing.T) {
	d := newStreamDecoder()
	evs, err := d.Feed("", `{"type":"error","error":{"code":"cyber_policy","message":"blocked by cyber policy"}}`)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(evs) != 1 || evs[0].Type != ir.EvError {
		t.Fatalf("want 恰好一个 EvError, got %+v", evs)
	}
	e := evs[0].Err
	if e.Kind != ir.ErrContentFilter {
		t.Errorf("Kind = %s, want content_filter", e.Kind)
	}
	if e.Retryable {
		t.Error("cyber_policy 必须不可重试：否则调度器换号重发，白烧账号池")
	}
	if e.Code != "cyber_policy" {
		t.Errorf("Code = %q, want 原文 cyber_policy（客户端据此判断该不该改内容）", e.Code)
	}
}

// content_policy_violation（OpenAI/Azure 审核族）走同一条特判，同样不可重试。
func TestStreamErrorContentPolicyViolationIsNonRetryable(t *testing.T) {
	d := newStreamDecoder()
	evs, err := d.Feed("", `{"type":"error","error":{"code":"content_policy_violation","message":"content moderation blocked this request"}}`)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(evs) != 1 || evs[0].Type != ir.EvError {
		t.Fatalf("want 恰好一个 EvError, got %+v", evs)
	}
	if evs[0].Err.Kind != ir.ErrContentFilter || evs[0].Err.Retryable {
		t.Errorf("content_policy_violation 归 %s / retryable=%v, want content_filter / false",
			evs[0].Err.Kind, evs[0].Err.Retryable)
	}
}

// 反向守卫：流内的限流码仍归 rate_limit 且可重试——风控特判没有把整张码表带偏。
func TestStreamErrorRateLimitStillRetryable(t *testing.T) {
	d := newStreamDecoder()
	evs, err := d.Feed("", `{"type":"error","error":{"code":"rate_limit_exceeded","message":"slow down"}}`)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(evs) != 1 || evs[0].Type != ir.EvError {
		t.Fatalf("want 恰好一个 EvError, got %+v", evs)
	}
	if evs[0].Err.Kind != ir.ErrRateLimit || !evs[0].Err.Retryable {
		t.Errorf("rate_limit_exceeded 归 %s / retryable=%v, want rate_limit / true",
			evs[0].Err.Kind, evs[0].Err.Retryable)
	}
}
