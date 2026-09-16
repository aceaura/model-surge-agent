package ir

import "testing"

// Retryable 由 Kind 单点推导，避免各调用点自行判断而互相矛盾。
func TestRetryableIsDerivedFromKind(t *testing.T) {
	cases := map[ErrorKind]bool{
		ErrRateLimit:       true,
		ErrUpstream:        true,
		ErrTimeout:         true,
		ErrInvalidRequest:  false,
		ErrAuth:            false,
		ErrNotFound:        false,
		ErrContextExceeded: false,
		ErrInternal:        false,
	}
	for kind, want := range cases {
		if got := NewError(kind, 0, "", "boom"); got.Retryable != want {
			t.Errorf("%s retryable = %v, want %v", kind, got.Retryable, want)
		}
	}
}

// 换个目标同样会超限，且这不该记作目标的失败，所以它必须不可重试。
func TestContextExceededIsNotRetryable(t *testing.T) {
	if NewError(ErrContextExceeded, 400, "", "too long").Retryable {
		t.Error("context_exceeded must not trigger a target swap")
	}
}

func TestErrorMessageIncludesUpstreamCode(t *testing.T) {
	withCode := NewError(ErrRateLimit, 429, "rate_limit_exceeded", "slow down")
	if got := withCode.Error(); got != "rate_limit: slow down (rate_limit_exceeded)" {
		t.Errorf("Error() = %q", got)
	}
	bare := NewError(ErrUpstream, 500, "", "boom")
	if got := bare.Error(); got != "upstream: boom" {
		t.Errorf("Error() = %q", got)
	}
}

func TestNilErrorStringIsSafe(t *testing.T) {
	var e *Error
	if got := e.Error(); got != "<nil>" {
		t.Errorf("Error() = %q", got)
	}
}
