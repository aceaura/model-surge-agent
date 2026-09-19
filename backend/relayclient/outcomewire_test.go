package relayclient_test

import (
	"testing"

	"github.com/aceaura/model-surge-agent/backend/relayclient"
)

// outcome 的字面值是跨进程契约：relay 的 runstate 按字符串匹配来决定
// 要不要递增失败计数、要不要冷却。两侧永远不互相 import（各自独立发版），
// 所以这一侧只能靠把字面值钉死来防漂移。
//
// 没有这一条，把 OutcomeTransport 的值改成 "normal" 在本仓内是完全自洽的——
// 所有测试都拿常量自身比较，全绿通过，而线上的效果是连接层故障被 relay
// 当成成功：清零失败计数、累计用量。本轮变异验证就是这样发现它的。
func TestOutcomeWireValues(t *testing.T) {
	cases := []struct {
		got  string
		want string
	}{
		{relayclient.OutcomeNormal, "normal"},
		{relayclient.OutcomeAbnormal, "abnormal"},
		{relayclient.OutcomeRetrying, "retrying"},
		{relayclient.OutcomeInvalidModel, "invalid_model"},
		{relayclient.OutcomeTransport, "transport"},
		{relayclient.OutcomeContextExceeded, "context_exceeded"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("outcome 字面值 = %q，要 %q："+
				"这是跨进程契约，改了它 relay 会按另一套语义更新运行态", c.got, c.want)
		}
	}
}

// 六个 outcome 两两不同。
//
// 撞车之后两类故障在 relay 侧走同一条分支，而运维在流水里也分不开它们。
func TestOutcomesAreDistinct(t *testing.T) {
	all := map[string]string{
		"normal":           relayclient.OutcomeNormal,
		"abnormal":         relayclient.OutcomeAbnormal,
		"retrying":         relayclient.OutcomeRetrying,
		"invalid_model":    relayclient.OutcomeInvalidModel,
		"transport":        relayclient.OutcomeTransport,
		"context_exceeded": relayclient.OutcomeContextExceeded,
	}
	seen := map[string]string{}
	for name, v := range all {
		if prev, dup := seen[v]; dup {
			t.Fatalf("%s 与 %s 的字面值都是 %q，两类故障会走同一条分支", name, prev, v)
		}
		seen[v] = name
	}
}
