package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 预算错误必须不可重试。
//
// 这条只能在包内断言：e2e 上这个取值被 SideEffectRisk 那条分支遮住了——
// 预算到期时请求早已完整交给上游，于是 pipeline 在读 Retryable 之前就先按
// 「有副作用风险」终止，两种取值在流水、上报与响应上完全同形。
// 换句话说，把它改成可重试今天不会有任何外部症状，而 MaxAttempts 与预算的
// 相对配置一变（比如首个目标在预算内就明确失败）它立刻会变成「没时间了
// 还去打下一个目标」。
func TestBudgetErrorIsNotRetryable(t *testing.T) {
	err := budgetError()
	if err.Retryable {
		t.Error("预算到期被标成可重试：换目标也没有时间了")
	}
	if err.Kind != ir.ErrTimeout {
		t.Errorf("kind = %q，want timeout", err.Kind)
	}
}

// teardownCause 的三种归因。
//
// 直接判这个函数而不只靠 e2e：桥接层走哪条拆流分支是竞态，e2e 夹具每次只能
// 命中其中一条，于是「另一条判错了」在跑一次的测试里是绿的。
func TestTeardownCauseAttribution(t *testing.T) {
	upErr := ir.NewError(ir.ErrUpstream, 0, "", "upstream stream ended")

	budget, cancel := context.WithTimeoutCause(
		context.Background(), time.Nanosecond, errRequestBudget)
	defer cancel()
	<-budget.Done()
	cause, gone := teardownCause(budget, nil, upErr)
	if gone {
		t.Error("预算到期被判成客户端自己走了")
	}
	if cause == nil || !strings.Contains(cause.Message, "budget") {
		t.Errorf("预算到期的错误不对：%+v", cause)
	}

	canceled, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if cause, gone := teardownCause(canceled, nil, upErr); !gone || cause != nil {
		t.Errorf("客户端取消没被认出来：cause=%+v gone=%v", cause, gone)
	}

	if cause, gone := teardownCause(context.Background(), nil, upErr); gone || cause != upErr {
		t.Errorf("上游拆流被归到了别人头上：cause=%+v gone=%v", cause, gone)
	}
}

// 流内错误优先于「流没有终止符就结束了」那条兜底。
//
// 上游先用错误帧说明了原因再断流时，那个原因才是根因。兜底错误的 kind 是
// upstream 且可重试，于是一次流内 429 会被记成目标故障累计它的失败计数，
// 错误帧带的退避秒数也一起丢掉——客户端拿不到 Retry-After。
func TestTeardownCausePrefersTheStreamError(t *testing.T) {
	upErr := ir.NewError(ir.ErrUpstream, 0, "", "upstream stream ended without a terminator")
	streamErr := ir.NewError(ir.ErrRateLimit, 429, "", "slow down")
	deadline := time.Now().Add(7 * time.Second)
	streamErr.RetryAfter = deadline

	cause, gone := teardownCause(context.Background(), streamErr, upErr)
	if gone {
		t.Error("流内错误被判成客户端自己走了")
	}
	if cause != streamErr {
		t.Fatalf("cause = %+v，want 流内那个错误——兜底错误会把限流记成目标故障", cause)
	}
	if !cause.RetryAfter.Equal(deadline) {
		t.Errorf("退避秒数丢了：%v", cause.RetryAfter)
	}

	// 本服务自己放弃或客户端走了，责任不在上游，哪怕上游此前说过什么。
	budget, cancel := context.WithTimeoutCause(
		context.Background(), time.Nanosecond, errRequestBudget)
	defer cancel()
	<-budget.Done()
	if cause, _ := teardownCause(budget, streamErr, upErr); cause == streamErr {
		t.Error("本服务的预算到期被记成了上游的错")
	}

	canceled, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if cause, gone := teardownCause(canceled, streamErr, upErr); !gone || cause != nil {
		t.Errorf("客户端取消让位给了流内错误：cause=%+v gone=%v", cause, gone)
	}

	// 上游没说过话时仍用兜底错误，不能因为多了一个参数就把兜底丢掉。
	if cause, _ := teardownCause(context.Background(), nil, upErr); cause != upErr {
		t.Errorf("没有流内错误时兜底归因丢了：%+v", cause)
	}
}

// budgetExceeded 只认本服务自己那个哨兵。
func TestBudgetExceededOnlyMatchesTheSentinel(t *testing.T) {
	budget, cancel := context.WithTimeoutCause(
		context.Background(), time.Nanosecond, errRequestBudget)
	defer cancel()
	<-budget.Done()
	if !budgetExceeded(budget) {
		t.Error("自己的预算没被认出来")
	}

	plain, cancel2 := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel2()
	<-plain.Done()
	if budgetExceeded(plain) {
		t.Error("一个普通的 deadline 被认成了预算到期")
	}

	canceled, cancel3 := context.WithCancel(context.Background())
	cancel3()
	if budgetExceeded(canceled) {
		t.Error("客户端取消被认成了预算到期")
	}

	if budgetExceeded(context.Background()) {
		t.Error("一个没到期的 ctx 被认成了预算到期")
	}
	if !errors.Is(context.Cause(budget), errRequestBudget) {
		t.Error("Cause 没带上哨兵")
	}
}
