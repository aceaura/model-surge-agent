package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// caller / toolset_name 是 anthropic tool_use 块上的发起方标记与 beta toolsets
// 归属名（官方 ToolUseBlock.caller 响应侧必填、ToolUseBlockParam.caller 请求侧
// 可选；toolset_name 两侧可选）。此前 tool_use 作为已知块型逐字段重建，未建模的
// 这两维会被静默丢掉——同族多轮历史里客户端回传上一轮 assistant 的 tool_use（带
// caller）时，往返不再逐字。这组测试钉住解码入 IR 与同族编码回吐。

func TestToolUseCallerAndToolsetDecodedIntoIR(t *testing.T) {
	body := `{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"SF"},` +
		`"caller":{"tool_id":"srvtoolu_9","type":"code_execution_20250825"},"toolset_name":"ts_beta"}`
	reqBody := `{"model":"claude-opus-5","messages":[{"role":"assistant","content":[` + body + `]}]}`
	req, err := DecodeRequest([]byte(reqBody))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	tu := req.Messages[0].Content[0].ToolUse
	if tu == nil {
		t.Fatalf("no ToolUse payload: %#v", req.Messages[0].Content[0])
	}
	if len(tu.Caller) == 0 {
		t.Fatalf("caller not carried into IR: %#v", tu)
	}
	if !strings.Contains(string(tu.Caller), "code_execution_20250825") {
		t.Fatalf("caller body = %s", tu.Caller)
	}
	if tu.ToolsetName != "ts_beta" {
		t.Fatalf("toolset_name = %q", tu.ToolsetName)
	}
}

func TestToolUseCallerRoundTripsVerbatim(t *testing.T) {
	body := `{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"SF"},` +
		`"caller":{"type":"direct"},"toolset_name":"ts_beta"}`
	reqBody := `{"model":"claude-opus-5","messages":[{"role":"assistant","content":[` + body + `]}]}`
	req, err := DecodeRequest([]byte(reqBody))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	wire, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	s := string(wire)
	if !strings.Contains(s, `"caller":{"type":"direct"}`) {
		t.Fatalf("caller lost on re-encode: %s", s)
	}
	if !strings.Contains(s, `"toolset_name":"ts_beta"`) {
		t.Fatalf("toolset_name lost on re-encode: %s", s)
	}
}

func TestServerToolUseCallerRoundTripsVerbatim(t *testing.T) {
	body := `{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"x"},` +
		`"caller":{"type":"direct"}}`
	reqBody := `{"model":"claude-opus-5","messages":[{"role":"assistant","content":[` + body + `]}]}`
	req, err := DecodeRequest([]byte(reqBody))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	stu := req.Messages[0].Content[0].ServerToolUse
	if stu == nil || len(stu.Caller) == 0 {
		t.Fatalf("server_tool_use caller not carried into IR: %#v", stu)
	}
	wire, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(wire), `"caller":{"type":"direct"}`) {
		t.Fatalf("server_tool_use caller lost on re-encode: %s", wire)
	}
	// server_tool_use 官方无 toolset_name，不应凭空写出该键。
	if strings.Contains(string(wire), "toolset_name") {
		t.Fatalf("server_tool_use should not emit toolset_name: %s", wire)
	}
}

func TestNoCallerOrToolsetWhenAbsent(t *testing.T) {
	body := `{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"SF"}}`
	reqBody := `{"model":"claude-opus-5","messages":[{"role":"assistant","content":[` + body + `]}]}`
	req, err := DecodeRequest([]byte(reqBody))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	wire, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if strings.Contains(string(wire), "caller") || strings.Contains(string(wire), "toolset_name") {
		t.Fatalf("absent caller/toolset_name should not emit keys: %s", wire)
	}
}

// 编码侧单测：直接从 IR 造带 caller/toolset_name 的 tool_use 块，验证 encodeBlock
// 逐字写出（覆盖不经解码器的构造路径，例如聚合后的流式响应块）。
func TestEncodeBlockWritesCallerFromIR(t *testing.T) {
	wb, ok, err := encodeBlock(ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
		ID: "toolu_1", Name: "f", Input: `{"a":1}`,
		Caller:      json.RawMessage(`{"tool_id":"srvtoolu_9","type":"code_execution_20260120"}`),
		ToolsetName: "ts_beta",
	}})
	if err != nil || !ok {
		t.Fatalf("encodeBlock: ok=%v err=%v", ok, err)
	}
	b, _ := json.Marshal(wb)
	s := string(b)
	if !strings.Contains(s, `"tool_id":"srvtoolu_9"`) || !strings.Contains(s, "code_execution_20260120") {
		t.Fatalf("caller not written verbatim: %s", s)
	}
	if !strings.Contains(s, `"toolset_name":"ts_beta"`) {
		t.Fatalf("toolset_name not written: %s", s)
	}
}
