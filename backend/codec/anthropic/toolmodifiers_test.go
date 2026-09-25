package anthropic

import (
	"strings"
	"testing"
)

// anthropic 工具定义 2026 修饰四维（defer_loading / eager_input_streaming /
// input_examples / allowed_callers）同族贯通：官方 SDK Tool 接口核对
// （2026-09-22）；其余协议没有这些槽位（跨族见 codec/toolmodifiers_test.go）。
//
// 对应旧仓 #27（a9d0095）。

func TestToolModifiersRoundTrip(t *testing.T) {
	body := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"name":"ping","input_schema":{"type":"object"},` +
		`"defer_loading":true,"eager_input_streaming":false,` +
		`"input_examples":[{"name":"x"}],"allowed_callers":["direct"]},` +
		`{"name":"plain","input_schema":{"type":"object"}}]}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if len(req.Tools) != 2 {
		t.Fatalf("tools = %d, want 2", len(req.Tools))
	}
	tl := req.Tools[0]
	if !tl.DeferLoading {
		t.Error("defer_loading 未解出")
	}
	if tl.EagerInputStreaming == nil || *tl.EagerInputStreaming {
		t.Errorf("eager_input_streaming 显式 false 应保真：%+v", tl.EagerInputStreaming)
	}
	if len(tl.InputExamples) != 1 || string(tl.InputExamples[0]) != `{"name":"x"}` {
		t.Errorf("input_examples = %s", tl.InputExamples)
	}
	if len(tl.AllowedCallers) != 1 || tl.AllowedCallers[0] != "direct" {
		t.Errorf("allowed_callers = %v", tl.AllowedCallers)
	}
	// 第二件工具全缺省。
	if p := req.Tools[1]; p.DeferLoading || p.EagerInputStreaming != nil ||
		len(p.InputExamples) > 0 || len(p.AllowedCallers) > 0 {
		t.Errorf("缺省工具解出了东西：%+v", p)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	s := string(out)
	for _, probe := range []string{`"defer_loading":true`, `"eager_input_streaming":false`,
		`"input_examples":[{"name":"x"}]`, `"allowed_callers":["direct"]`} {
		if !strings.Contains(s, probe) {
			t.Errorf("缺 %q: %s", probe, s)
		}
	}
	// plain 工具不得发明修饰键。
	if strings.Count(s, `"defer_loading"`) != 1 || strings.Count(s, `"eager_input_streaming"`) != 1 ||
		strings.Count(s, `"input_examples"`) != 1 || strings.Count(s, `"allowed_callers"`) != 1 {
		t.Errorf("缺省工具发明了修饰键: %s", s)
	}
}
