package anthropic

import (
	"encoding/json"
	"strings"
	"testing"
)

// 轮次85：anthropic 请求级 diagnostics（官方 MessageCreateParams.diagnostics =
// DiagnosticsParam{previous_message_id}）同族原文透传。客户端带上一轮响应的 msg_id
// 订阅 prompt-cache 失配归因，上游据此在响应回 diagnostics.cache_miss_reason。此前
// wireRequest 无此字段，入站带它会被静默丢（连 anthropic→anthropic 同族也丢，违同族
// 无损）。修法与 mcp_servers/context_management 完全同款：RawMessage 透传、显式 null
// 归一、同族回写、跨族由 DescribeLossy 报出（见 codec/round85diag_test.go）。
// 官方 SDK 对照：message_create_params.py:135 diagnostics、diagnostics_param.py
// previous_message_id（「Pass null on the first turn to opt in」）。

const diagReqPrefix = `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],`

func TestDiagnosticsDecodePassthrough(t *testing.T) {
	body := diagReqPrefix + `"diagnostics":{"previous_message_id":"msg_abc123"}}`
	r, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if len(r.Diagnostics) == 0 {
		t.Fatal("diagnostics 未落进 IR")
	}
	if !strings.Contains(string(r.Diagnostics), `"previous_message_id":"msg_abc123"`) {
		t.Errorf("diagnostics 原文漂移：%s", r.Diagnostics)
	}
	var probe any
	if err := json.Unmarshal(r.Diagnostics, &probe); err != nil {
		t.Errorf("diagnostics 非合法 JSON：%v", err)
	}
}

// 首轮 opt-in：previous_message_id 显式为 null，但外层 diagnostics 对象非空，
// 必须原样落进 IR（客户端确实订阅了诊断，只是无 prior 可比），不被归一掉。
func TestDiagnosticsDecodeFirstTurnNullOptIn(t *testing.T) {
	body := diagReqPrefix + `"diagnostics":{"previous_message_id":null}}`
	r, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if len(r.Diagnostics) == 0 {
		t.Fatalf("首轮 opt-in 的 diagnostics 被归一掉了，应原样收下：%q", r.Diagnostics)
	}
	if !strings.Contains(string(r.Diagnostics), "previous_message_id") {
		t.Errorf("diagnostics 原文漂移：%s", r.Diagnostics)
	}
}

func TestDiagnosticsDecodeAbsentAndNull(t *testing.T) {
	for _, body := range []string{
		// 完全缺席。
		`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`,
		// 显式 null（归一为没给，避免 4 字节字面量被当成配置回写）。
		diagReqPrefix + `"diagnostics":null}`,
	} {
		r, err := DecodeRequest([]byte(body))
		if err != nil {
			t.Fatalf("DecodeRequest: %v", err)
		}
		if len(r.Diagnostics) != 0 {
			t.Errorf("缺席/null 时 diagnostics 应为空：%s", r.Diagnostics)
		}
	}
}

func TestDiagnosticsEncodePassthrough(t *testing.T) {
	in := diagReqPrefix + `"diagnostics":{"previous_message_id":"msg_abc123"}}`
	r, err := DecodeRequest([]byte(in))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	out, err := EncodeRequest(r)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(out), `"diagnostics"`) ||
		!strings.Contains(string(out), `"previous_message_id":"msg_abc123"`) {
		t.Errorf("diagnostics 未原样回写：%s", out)
	}
}

func TestDiagnosticsEncodeAbsentNoKey(t *testing.T) {
	r, err := DecodeRequest([]byte(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	out, err := EncodeRequest(r)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if strings.Contains(string(out), "diagnostics") {
		t.Errorf("缺席不应出 diagnostics 键：%s", out)
	}
}

// 同族往返：decode→encode→decode 原文不漂移。
func TestDiagnosticsRequestRoundTrip(t *testing.T) {
	in := diagReqPrefix + `"diagnostics":{"previous_message_id":"msg_xyz789"}}`
	r, err := DecodeRequest([]byte(in))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	out, err := EncodeRequest(r)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	back, err := DecodeRequest(out)
	if err != nil {
		t.Fatalf("DecodeRequest(往返): %v", err)
	}
	if !strings.Contains(string(back.Diagnostics), `"previous_message_id":"msg_xyz789"`) {
		t.Errorf("往返 diagnostics 漂移：%s", back.Diagnostics)
	}
}
