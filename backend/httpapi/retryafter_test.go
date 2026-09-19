package httpapi

import (
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/codec/anthropic"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 受理面与路由层的拒绝同样要回 Retry-After。
//
// 这条路径与数据面终态是两个独立出口，改一处不会带上另一处。
func TestRejectionCarriesRetryAfterHeader(t *testing.T) {
	err := ir.NewError(ir.ErrRateLimit, 429, "rate_limit", "slow down")
	err.RetryAfter = time.Now().Add(42 * time.Second)

	w := httptest.NewRecorder()
	writeIRError(w, anthropicInbound(t), err)

	raw, ok := sentRetryAfter(w)
	if !ok {
		t.Fatal("拒绝响应没带 Retry-After")
	}
	secs, convErr := strconv.Atoi(raw)
	if convErr != nil {
		t.Fatalf("Retry-After = %q，不是整数秒: %v", raw, convErr)
	}
	// 向上取整，所以 42.0 秒会落在 42 或 43。
	if secs < 41 || secs > 43 {
		t.Errorf("Retry-After = %d, want 约 42", secs)
	}
}

// 零值 RetryAfter 不发这个头。绝大多数错误（400、500）没有这一维度，
// 给它们发一个头等于让客户端按一个编出来的时长退避。
func TestRejectionWithoutRetryAfterHasNoHeader(t *testing.T) {
	w := httptest.NewRecorder()
	writeIRError(w, anthropicInbound(t), ir.NewError(ir.ErrInvalidRequest, 400, "", "bad"))

	if got, ok := sentRetryAfter(w); ok {
		t.Errorf("没有到期时刻却发了 Retry-After: %q", got)
	}
}

// 已过去的时刻不发：一个 0 或负数会让 SDK 立刻重来，比不发更坏。
func TestRejectionWithPastRetryAfterHasNoHeader(t *testing.T) {
	err := ir.NewError(ir.ErrRateLimit, 429, "", "slow down")
	err.RetryAfter = time.Now().Add(-time.Minute)

	w := httptest.NewRecorder()
	writeIRError(w, anthropicInbound(t), err)

	if got, ok := sentRetryAfter(w); ok {
		t.Errorf("过期的到期时刻仍发了 Retry-After: %q", got)
	}
}

// 没有入站 codec 的那条兜底路径也要走同一套。
//
// 它此前直接 writeJSON，是一条独立的分支：只改有 codec 的那支会让
// 「连协议都没认出来」的限流拒绝拿不到头。
func TestFallbackEnvelopeAlsoCarriesRetryAfter(t *testing.T) {
	err := ir.NewError(ir.ErrRateLimit, 429, "", "slow down")
	err.RetryAfter = time.Now().Add(30 * time.Second)

	w := httptest.NewRecorder()
	writeIRErrorStatus(w, nil, err, 429)

	if _, ok := sentRetryAfter(w); !ok {
		t.Error("无 codec 的兜底信封没带 Retry-After")
	}
}

// sentRetryAfter 读的是「实际发出去的」响应头，不是 recorder 的可写 map。
//
// 两点都必须这样读：
//   - Result().Header 只含 WriteHeader 之前设的头。直接读 w.Header() 的话，
//     一个「写头之后才设 Retry-After」的实现在测试里照样绿，而真客户端
//     一个字节都收不到。
//   - 键存在且值为空串与键不存在是两回事，Get 分不出来。一个把空值写进去
//     的实现会让 `Get() != ""` 形式的断言全部漏过。
func sentRetryAfter(w *httptest.ResponseRecorder) (string, bool) {
	vs, ok := w.Result().Header["Retry-After"]
	if !ok || len(vs) == 0 {
		return "", false
	}
	return vs[0], true
}

// setRetryAfter 必须容忍 nil 错误：调用点不该为此各自判一次。
func TestSetRetryAfterToleratesNil(t *testing.T) {
	w := httptest.NewRecorder()
	if setRetryAfter(w, nil, time.Now()) {
		t.Error("nil 错误上报告说写了头")
	}
}

// anthropicInbound 从注册表取入站 codec。直接构造具体类型会把测试钉在
// 那个类型的名字上，而注册表是生产路径实际用的取法。
func anthropicInbound(t *testing.T) codec.InboundCodec {
	t.Helper()
	in, ok := codec.Inbound(anthropic.Name)
	if !ok {
		t.Fatalf("入站 codec %q 没注册", anthropic.Name)
	}
	return in
}
