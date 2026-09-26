package pipeline

import (
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
)

// outcomeFor 是 IR 分类 → 调度层 outcome 的落点。P1/P2/P3 改的是 errkind.go 的
// 分类，但真正影响调度行为的是这里的映射：retrying 会换目标重发，abnormal 就地
// 报错。三项都复用现有 outcome 枚举值——relay 契约不动，变的只是哪一类错误落哪一档。
func TestOutcomeForPBatch(t *testing.T) {
	cases := []struct {
		name string
		kind ir.ErrorKind
		want string
	}{
		// P1：402 欠费 → rate_limit → 换号重试。
		{"P1 欠费换号", ir.ErrRateLimit, relayclient.OutcomeRetrying},
		// P2：408/425 → timeout → 换目标重试。
		{"P2 超时换目标", ir.ErrTimeout, relayclient.OutcomeRetrying},
		// P3：风控 → content_filter → 就地报错，不换号（换号也照样被挡）。
		{"P3 风控就地报错", ir.ErrContentFilter, relayclient.OutcomeAbnormal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := outcomeFor(ir.NewError(c.kind, 0, "", "x"))
			if got != c.want {
				t.Errorf("outcomeFor(%s) = %s, want %s", c.kind, got, c.want)
			}
		})
	}
}

// content_filter 落 outcomeFor 的 default 分支（abnormal），而不是被误并进
// retrying 那一组——这正是 P3 要堵的「换号硬撞风控墙」缺口。
func TestContentFilterIsNotRetrying(t *testing.T) {
	if got := outcomeFor(ir.NewError(ir.ErrContentFilter, 0, "cyber_policy", "blocked")); got == relayclient.OutcomeRetrying {
		t.Error("content_filter 不得归 retrying：那会让调度器换号重发一个永远被挡的请求")
	}
}
