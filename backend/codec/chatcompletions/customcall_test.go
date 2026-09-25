package chatcompletions

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// chat 本族原生 custom tool call（type=custom，custom{name,input}）：
// 此前只读 function 槽位，custom 调用被伪造成空名函数调用，客户端按
// 函数名路由必然落空；编码侧则把自由文本包成 {"input":…} 投影。

func TestCustomToolCallDecode(t *testing.T) {
	// 请求侧历史。
	body := `{"model":"m","messages":[{"role":"assistant","tool_calls":[` +
		`{"id":"call_1","type":"custom","custom":{"name":"shell","input":"echo hi"}}]}]}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	var tu *ir.ToolUse
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockToolUse {
				tu = b.ToolUse
			}
		}
	}
	if tu == nil || tu.Kind != ir.ToolCustom || tu.Name != "shell" || tu.InputText != "echo hi" {
		t.Fatalf("custom 调用解码不对: %+v", tu)
	}
	if tu.Input != `{"input":"echo hi"}` {
		t.Errorf("Input 投影 = %q", tu.Input)
	}

	// 非流式响应侧同款。
	respBody := `{"id":"c1","model":"m","choices":[{"index":0,"finish_reason":"tool_calls",` +
		`"message":{"role":"assistant","tool_calls":[{"id":"call_9","type":"custom","custom":{"name":"shell","input":"ls"}}]}}]}`
	resp, err := DecodeResponse([]byte(respBody))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(resp.Content) != 1 || resp.Content[0].ToolUse == nil ||
		resp.Content[0].ToolUse.Kind != ir.ToolCustom ||
		resp.Content[0].ToolUse.Name != "shell" ||
		resp.Content[0].ToolUse.InputText != "ls" {
		t.Fatalf("响应侧 custom 调用解码不对: %+v", resp.Content)
	}
}

// 编码侧：custom 调用回本族原生形态，不得投影成函数、不得带空 function
// 键；函数调用形态不变。
func TestCustomToolCallEncodeNative(t *testing.T) {
	req := &ir.Request{Model: "m", Messages: []ir.Message{{
		Role: ir.RoleAssistant,
		Content: []ir.Block{
			{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
				ID: "call_1", Name: "shell", Kind: ir.ToolCustom,
				InputText: "echo hi", Input: `{"input":"echo hi"}`,
			}},
			{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
				ID: "call_2", Name: "get", Input: `{"q":1}`,
			}},
		},
	}}}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"type":"custom","custom":{"name":"shell","input":"echo hi"}`) {
		t.Errorf("custom 调用没走原生形态: %s", s)
	}
	if strings.Contains(s, `"function":{"name":"","arguments":""}`) ||
		strings.Contains(s, `"custom":{"name":"shell","input":"echo hi"},"function"`) {
		t.Errorf("custom 调用泄漏了空 function 键: %s", s)
	}
	if !strings.Contains(s, `"type":"function","function":{"name":"get","arguments":"{\"q\":1}"}`) {
		t.Errorf("函数调用形态变了: %s", s)
	}
}

// 废弃 function_call 三处不再静默蒸发：请求/非流式载荷完整直接进 IR；
// tool_calls 同在时以 tool_calls 为准，两槽位不重复进 IR。
func TestDeprecatedFunctionCallDecode(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"assistant","function_call":{"name":"legacy","arguments":"{\"a\":1}"}}]}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	var name, args string
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockToolUse && b.ToolUse != nil {
				name, args = b.ToolUse.Name, b.ToolUse.Input
			}
		}
	}
	if name != "legacy" || args != `{"a":1}` {
		t.Fatalf("请求侧废弃 function_call 蒸发: %q %q", name, args)
	}

	respBody := `{"id":"c1","model":"m","choices":[{"index":0,"finish_reason":"function_call",` +
		`"message":{"role":"assistant","function_call":{"name":"legacy","arguments":"{\"b\":2}"}}}]}`
	resp, err := DecodeResponse([]byte(respBody))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(resp.Content) != 1 || resp.Content[0].ToolUse == nil ||
		resp.Content[0].ToolUse.Name != "legacy" || resp.Content[0].ToolUse.Input != `{"b":2}` {
		t.Fatalf("响应侧废弃 function_call 蒸发: %+v", resp.Content)
	}

	both := `{"model":"m","messages":[{"role":"assistant",` +
		`"tool_calls":[{"id":"call_1","type":"function","function":{"name":"modern","arguments":"{}"}}],` +
		`"function_call":{"name":"legacy","arguments":"{}"}}]}`
	req2, err := DecodeRequest([]byte(both))
	if err != nil {
		t.Fatalf("DecodeRequest(both): %v", err)
	}
	n := 0
	for _, m := range req2.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockToolUse {
				n++
				if b.ToolUse.Name != "modern" {
					t.Errorf("tool_calls 应优先：%q", b.ToolUse.Name)
				}
			}
		}
	}
	if n != 1 {
		t.Errorf("两槽位同在应只进一条：%d", n)
	}
}

// 流式碎片（delta.function_call）：name/arguments 并入 index 0 的 pending
// 轨道聚合成完整调用，id 收尾合成并经注记报出。
func TestDeprecatedFunctionCallStream(t *testing.T) {
	resp, notes := decodeStream(t,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"function_call":{"name":"legacy"}}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"function_call":{"arguments":"{\"a\":"}}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"function_call":{"arguments":"1}"}}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"function_call"}]}`,
		doneSentinel,
	)
	tools := toolBlocks(resp)
	if len(tools) != 1 {
		t.Fatalf("tool blocks = %d, want 1: %+v", len(tools), tools)
	}
	if tools[0].ToolUse.Name != "legacy" || tools[0].ToolUse.Input != `{"a":1}` {
		t.Errorf("流式废弃 function_call 丢失: name=%q input=%q",
			tools[0].ToolUse.Name, tools[0].ToolUse.Input)
	}
	if tools[0].ToolUse.ID == "" {
		t.Errorf("收尾没合成 id")
	}
	if !anyNoteHas(notes, "synthesized an id") {
		t.Errorf("合成 id 注记不对：%q", notes)
	}
}
