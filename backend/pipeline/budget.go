package pipeline

import (
	"context"
	"errors"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// errRequestBudget 是总时长上限到期时挂在 ctx 上的 Cause。
//
// 不导出：它只是一个身份标记，判定一律走本文件的两个函数。导出会让调用方
// 自己写 errors.Is，而那条判断有一半的写法（比较 ctx.Err()、比较 Unwrap 后的
// 值）恒为假且不会变红。
var errRequestBudget = errors.New("request duration budget exceeded")

// budgetExceeded 判这个 ctx 是不是因为本服务的预算到期而结束的。
//
// 客户端取消与预算到期的 ctx.Err() 完全相同（都是 context.Canceled 或
// DeadlineExceeded），唯一的区别在 Cause 上。
func budgetExceeded(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errRequestBudget)
}

// teardownCause 判流被拆掉的责任方，给出要报的错以及「是否客户端自己走了」。
//
// 单一决策点而不是在每条终止分支上重复判断：桥接层有两条拆流分支（帧 channel
// 干净关闭、scanner 报读错误），上游被掐断时走哪条取决于读协程当时停在哪里，
// 是竞态。两处各写一遍的话其中一处判错只在另一次运行里才暴露——而那种缺陷
// 在跑一次的测试里是绿的。
// streamErr 是上游在流内已经说明过的错误（有就说明它自己交代了原因），
// upstreamErr 是「流没了但没人说为什么」的兜底归因。
func teardownCause(ctx context.Context, streamErr, upstreamErr *ir.Error) (cause *ir.Error, clientGone bool) {
	// 预算判在客户端取消之前：预算到期时 ctx.Err() 也非 nil，
	// 顺序反了的话预算分支永远走不到。
	if budgetExceeded(ctx) {
		return budgetError(), false
	}
	if ctx.Err() != nil {
		return nil, true
	}
	// 流内错误优先于通用断流错误：上游先说明了原因再断，那个原因才是根因。
	// 此前这两条断流分支只报「流没有终止符就结束了」，于是一次流内 429
	// 被记成目标故障并累计它的失败计数，而错误帧带的退避秒数一起丢掉——
	// 客户端拿不到 Retry-After，运维看到的是一次上游抖动。
	//
	// 排在预算与客户端取消之后：本服务自己放弃或客户端走了，那是我们/客户端
	// 的责任，不该记成上游的错，哪怕上游此前确实说过什么。
	if streamErr != nil {
		return streamErr, false
	}
	return upstreamErr, false
}

// budgetError 是预算到期时交出去的错误。
//
// 不可重试：ErrTimeout 那一族默认是可重试的，而预算到期恰恰意味着没有时间再
// 换一个目标了——标成可重试会让最后那点预算全花在注定超时的重试上。
func budgetError() *ir.Error {
	err := ir.NewError(ir.ErrTimeout, 0, "",
		"request exceeded the total duration budget (MSA_MAX_REQUEST_DURATION)")
	err.Retryable = false
	return err
}
