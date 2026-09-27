package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// caller 是 web_search_tool_result 响应侧必填、请求侧可选的发起方标记
// （union）。同族往返必须逐字保留——丢了就等于把「哪一方发起的这次搜索」
// 这条溯源信息抹掉。以下用例覆盖：结果数组形态、错误 union 形态、缺省不_emit。

// 结果数组形态：decode 读入 caller，encode 原样回吐。
func TestWebSearchToolResultCallerRoundTripsVerbatim(t *testing.T) {
	caller := json.RawMessage(`{"type":"code_execution_20250825","tool_id":"srvtoolu_1"}`)
	in := wireBlock{Type: blockWebSearchToolResult, ToolUseID: "srvtoolu_1",
		Caller:  caller,
		Content: json.RawMessage(`[{"type":"web_search_result","title":"T","url":"https://e.com","encrypted_content":"s"}]`)}

	b, ok, err := decodeBlock(in, nil)
	if err != nil || !ok {
		t.Fatalf("decodeBlock: ok=%v err=%v", ok, err)
	}
	if b.Type != ir.BlockWebSearchToolResult || b.WebSearchToolResult == nil {
		t.Fatalf("block = %#v", b)
	}
	if string(b.WebSearchToolResult.Caller) != string(caller) {
		t.Errorf("decoded caller = %s, want %s", b.WebSearchToolResult.Caller, caller)
	}
	if len(b.WebSearchToolResult.Results) != 1 {
		t.Errorf("results = %#v", b.WebSearchToolResult.Results)
	}

	out, ok, err := encodeBlock(b)
	if err != nil || !ok {
		t.Fatalf("encodeBlock: ok=%v err=%v", ok, err)
	}
	if out.Type != blockWebSearchToolResult || out.ToolUseID != "srvtoolu_1" {
		t.Errorf("wire = %#v", out)
	}
	if string(out.Caller) != string(caller) {
		t.Errorf("encoded caller = %s, want %s", out.Caller, caller)
	}
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), `"caller":{"type":"code_execution_20250825"`) {
		t.Errorf("wire json missing caller: %s", raw)
	}
}

// 错误 union 形态：搜索失败也要带 caller，编回 web_search_tool_result_error
// 的同时不能丢发起方标记。
func TestWebSearchToolResultCallerRoundTripsInErrorForm(t *testing.T) {
	caller := json.RawMessage(`{"type":"direct"}`)
	in := wireBlock{Type: blockWebSearchToolResult, ToolUseID: "srvtoolu_2",
		Caller:  caller,
		Content: json.RawMessage(`{"type":"web_search_tool_result_error","error_code":"max_uses_exceeded"}`)}

	b, ok, err := decodeBlock(in, nil)
	if err != nil || !ok {
		t.Fatalf("decodeBlock: ok=%v err=%v", ok, err)
	}
	wsr := b.WebSearchToolResult
	if wsr == nil || wsr.ErrorCode != "max_uses_exceeded" {
		t.Fatalf("block = %#v", b)
	}
	if string(wsr.Caller) != string(caller) {
		t.Errorf("decoded caller = %s, want %s", wsr.Caller, caller)
	}

	out, ok, err := encodeBlock(b)
	if err != nil || !ok {
		t.Fatalf("encodeBlock: ok=%v err=%v", ok, err)
	}
	if string(out.Caller) != string(caller) {
		t.Errorf("encoded caller = %s, want %s", out.Caller, caller)
	}
	if !strings.Contains(string(out.Content), `"web_search_tool_result_error"`) {
		t.Errorf("content lost error union: %s", out.Content)
	}
}

// 缺省：请求侧 caller 可选，缺席时 IR 留空、编码不_emit caller 键。
func TestWebSearchToolResultNoCallerWhenAbsent(t *testing.T) {
	in := wireBlock{Type: blockWebSearchToolResult, ToolUseID: "srvtoolu_3",
		Content: json.RawMessage(`[{"type":"web_search_result","title":"T","url":"https://e.com"}]`)}

	b, ok, err := decodeBlock(in, nil)
	if err != nil || !ok {
		t.Fatalf("decodeBlock: ok=%v err=%v", ok, err)
	}
	if b.WebSearchToolResult == nil || len(b.WebSearchToolResult.Caller) != 0 {
		t.Errorf("caller should be empty, block = %#v", b)
	}

	out, ok, err := encodeBlock(b)
	if err != nil || !ok {
		t.Fatalf("encodeBlock: ok=%v err=%v", ok, err)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), `"caller"`) {
		t.Errorf("wire json should omit caller: %s", raw)
	}
}
