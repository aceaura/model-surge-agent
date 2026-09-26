package anthropic

import (
	"strings"
	"testing"
)

// 未知 thinking.type 必须 400，不能静默落进 disabled：那会把「客户端要求思考」
// 改写成「明令模型别思考」，出站编码器还会忠实地把 disabled 写回去。
func TestUnknownThinkingTypeRejected(t *testing.T) {
	for _, typ := range []string{"Enabled", "on", "true", "auto", "adaptive "} {
		body := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],` +
			`"thinking":{"type":"` + typ + `"}}`
		_, err := DecodeRequest([]byte(body))
		if err == nil {
			t.Errorf("thinking.type=%q 应被拒收，却通过了", typ)
			continue
		}
		if !strings.Contains(err.Error(), "thinking.type") {
			t.Errorf("thinking.type=%q 错误未点名 thinking.type：%v", typ, err)
		}
	}
}

// 三种合法取值都要通过，且语义正确。
func TestKnownThinkingTypesAccepted(t *testing.T) {
	cases := map[string]struct {
		on       bool
		adaptive bool
	}{
		"enabled":  {on: true, adaptive: false},
		"adaptive": {on: true, adaptive: true},
		"disabled": {on: false, adaptive: false},
	}
	for typ, want := range cases {
		body := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],` +
			`"thinking":{"type":"` + typ + `"}}`
		req, err := DecodeRequest([]byte(body))
		if err != nil {
			t.Errorf("thinking.type=%q 合法却被拒：%v", typ, err)
			continue
		}
		if req.Thinking == nil {
			t.Errorf("thinking.type=%q 未进 IR", typ)
			continue
		}
		if req.Thinking.On() != want.on {
			t.Errorf("thinking.type=%q On()=%v，want %v", typ, req.Thinking.On(), want.on)
		}
		if req.Thinking.Adaptive != want.adaptive {
			t.Errorf("thinking.type=%q Adaptive=%v，want %v", typ, req.Thinking.Adaptive, want.adaptive)
		}
	}
}
