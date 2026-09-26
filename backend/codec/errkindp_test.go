package codec_test

import (
	"net/http"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// P1/P2/P3 改的都是同一张「上游错误 → IR 分类」判据表（errkind.go）。
// 分类决定 Retryable，Retryable 经 pipeline.outcomeFor 决定调度层 outcome：
// 换目标重试（retrying）还是就地报错（abnormal）。三项都复用现有 outcome
// 枚举值，relay 契约不动——变的只是归因口径。

// P1：402 欠费归 rate_limit 族（换号），不再落 default=invalid_request（不换号、白烧当前账号）。
func TestP1Status402IsRateLimit(t *testing.T) {
	if got := codec.KindForStatus(http.StatusPaymentRequired, ""); got != ir.ErrRateLimit {
		t.Errorf("KindForStatus(402) = %s, want %s；欠费账号本该换号重试", got, ir.ErrRateLimit)
	}
	if !ir.NewError(codec.KindForStatus(http.StatusPaymentRequired, ""), 402, "", "no credit").Retryable {
		t.Error("402 归类后必须可重试（换号）")
	}
}

// P2：408/425 归 timeout 族（换目标），不再落 default=invalid_request（不重试）。
func TestP2Status408And425AreTimeout(t *testing.T) {
	for _, status := range []int{http.StatusRequestTimeout, http.StatusTooEarly} {
		if got := codec.KindForStatus(status, ""); got != ir.ErrTimeout {
			t.Errorf("KindForStatus(%d) = %s, want %s", status, got, ir.ErrTimeout)
		}
		if !ir.NewError(codec.KindForStatus(status, ""), status, "", "x").Retryable {
			t.Errorf("%d 归类后必须可重试（换目标）", status)
		}
	}
}

// P3：流内风控码归 content_filter（不可重试），不再落 default=upstream（换号硬撞、白烧账号池）。
func TestP3RiskControlCodesAreContentFilter(t *testing.T) {
	for _, code := range []string{
		"cyber_policy", "content_policy", "content_policy_violation", "content_filter",
	} {
		// status=0 是流内错误帧的形态：没有 HTTP 状态码，只有 code 可依据。
		if got := codec.KindFor(0, code, "blocked by policy"); got != ir.ErrContentFilter {
			t.Errorf("KindFor(0,%q) = %s, want %s", code, got, ir.ErrContentFilter)
		}
		if ir.NewError(codec.KindFor(0, code, ""), 0, code, "").Retryable {
			t.Errorf("风控码 %q 必须不可重试：换号重发一个永远被挡的请求会白烧账号池", code)
		}
	}
}

// P3 反向守卫：风控特判不得误伤普通参数错误与限流——它们仍走各自的重试语义。
func TestP3DoesNotOverreach(t *testing.T) {
	if got := codec.KindFor(0, "invalid_request_error", "bad param"); got != ir.ErrInvalidRequest {
		t.Errorf("普通参数错误 = %s, want invalid_request", got)
	}
	if got := codec.KindFor(0, "rate_limit_exceeded", "slow down"); got != ir.ErrRateLimit {
		t.Errorf("限流码 = %s, want rate_limit", got)
	}
}

// content_filter 作「错误码」不可重试；作「终止原因」则是正常收尾（StopContentFilter）——
// 两条路径不混淆。这里守错误码这一侧的状态码反查：安全拦截是客户端内容问题，归 400。
func TestP3ContentFilterStatus(t *testing.T) {
	if got := codec.StatusForKind(ir.ErrContentFilter); got != http.StatusBadRequest {
		t.Errorf("StatusForKind(content_filter) = %d, want 400", got)
	}
}
