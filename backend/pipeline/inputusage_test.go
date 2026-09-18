package pipeline_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
	"github.com/aceaura/model-surge-agent/backend/pipeline"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
	"github.com/aceaura/model-surge-agent/backend/relaymock"
)

// 本文件守的是 input 侧的用量兜底。
//
// 只兜 output 的话，上游不报 usage 时整个输入维度恒为 0，调度层的用量累计
// 会系统性漏掉它——而输入侧通常是两者中更大的那一维。

// streamWithoutUsage 是一段正常收尾但完全不带 usage 的流。
const streamWithoutUsage = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"answer"}}

event: message_stop
data: {"type":"message_stop"}

`

// streamWithInputUsage 带一个明确的 input_tokens，用来验证不被覆盖。
const streamWithInputUsage = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":4242}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"answer"}}

event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}

`

func serveWithStream(t *testing.T, raw string, estimate bool) *fixture {
	t.Helper()
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, raw)
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})
	f.p.Opts.EstimateUsage = estimate
	f.p.Serve(context.Background(), httptest.NewRecorder(), call(t, true))
	return f
}

// 上游不报 input_tokens 时按估算填入。
func TestInputTokensAreEstimatedWhenUpstreamOmitsThem(t *testing.T) {
	rec := serveWithStream(t, streamWithoutUsage, true).col.record(t)
	if rec.Usage.InputTokens == 0 {
		t.Error("input_tokens 仍为 0，调度层的用量累计会永远漏掉输入侧")
	}
	if !rec.UsageEstimated {
		t.Error("兜底了却没标记，统计读不出这行数字含估算成分")
	}
}

// 上游报了就原样透传，不得被估算覆盖。
//
// 覆盖的后果比不兜底更糟：上游的真实数字是唯一权威，用估算盖掉它会让
// 账目与上游对不上，而且看不出是谁改的。
func TestUpstreamInputTokensAreNotOverwritten(t *testing.T) {
	rec := serveWithStream(t, streamWithInputUsage, true).col.record(t)
	if rec.Usage.InputTokens != 4242 {
		t.Errorf("input_tokens = %d, want 4242（上游报的值必须原样透传）",
			rec.Usage.InputTokens)
	}
}

// 关掉估算开关时两个维度都不兜。
func TestEstimateSwitchOffLeavesBothDimensionsAlone(t *testing.T) {
	rec := serveWithStream(t, streamWithoutUsage, false).col.record(t)
	if rec.Usage.InputTokens != 0 || rec.Usage.OutputTokens != 0 {
		t.Errorf("关掉估算后仍被兜底：in=%d out=%d",
			rec.Usage.InputTokens, rec.Usage.OutputTokens)
	}
	if rec.UsageEstimated {
		t.Error("没估算却标了 usage_estimated")
	}
}

// usage_estimated 表达「这行数字里有估算成分」，不区分是哪一维。
//
// 上游只报了 output 而没报 input 时，标记仍必须为 true——否则统计会把
// 一半是估算的行当成完全可信。
func TestUsageEstimatedFlagsPartialEstimation(t *testing.T) {
	const onlyOutput = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"answer"}}

event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":9}}

event: message_stop
data: {"type":"message_stop"}

`
	rec := serveWithStream(t, onlyOutput, true).col.record(t)
	if rec.Usage.OutputTokens != 9 {
		t.Errorf("output_tokens = %d, want 9（上游报了就不该被覆盖）",
			rec.Usage.OutputTokens)
	}
	if rec.Usage.InputTokens == 0 {
		t.Error("input 侧没被兜底")
	}
	if !rec.UsageEstimated {
		t.Error("只有一维是估算时标记也必须为 true")
	}
}

// 兜底取调度方向（低估），不取公开方向。
//
// 这个数字进的是配额累计，高估等于凭空吃掉用户的额度。
func TestInputFallbackUsesDispatchDirection(t *testing.T) {
	rec := serveWithStream(t, streamWithoutUsage, true).col.record(t)
	req := call(t, true).Request
	dispatch := ir.EstimateRequest(req)
	public := ir.EstimateRequestMode(req, ir.ModePublic)
	if rec.Usage.InputTokens != dispatch {
		t.Errorf("input_tokens = %d，want 调度方向 %d（公开方向是 %d）",
			rec.Usage.InputTokens, dispatch, public)
	}
}

// 新增的上下文超限文案要能一路走到 context_exceeded 结局。
//
// 这条是端到端守卫：文案表补了、但若中间哪一层把它归到别处，运维侧看到的
// 仍是 abnormal——那会累计目标的失败计数并可能冷却一个健康账号，
// 而真正的原因是这一次请求太长，换任何账号都一样。
func TestNewOverflowPhrasingReachesContextExceededOutcome(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(
			`{"type":"error","error":{"type":"invalid_request_error",` +
				`"message":"Request is too long"}}`))
	}}
	url := up.start(t)
	f := newFixture(t,
		relaymock.Step{Target: target(url, "kimi-1/k3")},
		relaymock.Step{Target: target(url, "ark-1/ds")},
	)

	f.p.Serve(context.Background(), httptest.NewRecorder(), call(t, true))

	got := f.col.outcomes()
	if len(got) != 1 || got[0] != relayclient.OutcomeContextExceeded {
		t.Errorf("outcomes = %v，want [%s]：请求太长换账号也没用，"+
			"记成 abnormal 会冷却健康账号", got, relayclient.OutcomeContextExceeded)
	}
	if calls := up.calls(); calls != 1 {
		t.Errorf("上游被打了 %d 次，请求太长不该换目标重试", calls)
	}
}

// cjkCall 是一个中文请求：中文下两个方向的差距足够大，
// 用 ASCII 的话两者可能取整到同一个数，守卫就成了摆设。
func cjkCall(t *testing.T) pipeline.Call {
	t.Helper()
	c := call(t, true)
	c.Request.Messages = []ir.Message{{
		Role: ir.RoleUser,
		Content: []ir.Block{{
			Type: ir.BlockText,
			Text: strings.Repeat("这是一段中文提示内容", 20),
		}},
	}}
	return c
}

// dispatch 的 est_tokens 必须是调度方向。
//
// 它给策略脚本按上下文窗口筛候选：高估会让本装得下的请求被排掉所有目标，
// 客户端拿到的是「无可用目标」而非一个回答。
func TestEstTokensUsesDispatchDirection(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, streamWithoutUsage)
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})

	c := cjkCall(t)
	f.p.Serve(context.Background(), httptest.NewRecorder(), c)

	dispatches := f.relay.Dispatches()
	if len(dispatches) != 1 {
		t.Fatalf("dispatch 次数 = %d", len(dispatches))
	}
	wantDispatch := int(ir.EstimateRequest(c.Request))
	wantPublic := int(ir.EstimateRequestMode(c.Request, ir.ModePublic))
	if wantDispatch == wantPublic {
		t.Fatal("两个方向在这条请求上取值相同，守卫失效——换一条差距更大的输入")
	}
	if got := dispatches[0].EstTokens; got != wantDispatch {
		t.Errorf("est_tokens = %d，want 调度方向 %d（公开方向是 %d）："+
			"高估会让本装得下的请求被排掉所有目标", got, wantDispatch, wantPublic)
	}
}

// 用量兜底也必须是调度方向，理由不同：这个数字进配额累计，高估等于
// 凭空吃掉用户的额度。用中文请求把两个方向拉开才测得出来。
func TestInputFallbackStaysOnDispatchDirectionForCJK(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, streamWithoutUsage)
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})
	f.p.Opts.EstimateUsage = true

	c := cjkCall(t)
	f.p.Serve(context.Background(), httptest.NewRecorder(), c)

	wantDispatch := ir.EstimateRequest(c.Request)
	wantPublic := ir.EstimateRequestMode(c.Request, ir.ModePublic)
	if wantDispatch == wantPublic {
		t.Fatal("两个方向在这条请求上取值相同，守卫失效")
	}
	if got := f.col.record(t).Usage.InputTokens; got != wantDispatch {
		t.Errorf("兜底的 input_tokens = %d，want 调度方向 %d（公开方向是 %d）："+
			"高估会凭空吃掉用户配额", got, wantDispatch, wantPublic)
	}
}
