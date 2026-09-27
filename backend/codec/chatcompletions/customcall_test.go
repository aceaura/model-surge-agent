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

// 流式 custom 调用（delta.tool_calls type=custom）：此前流式只读 function 槽位，
// custom 调用因 function.name 恒空被兜底成 unknown_tool、自由文本入参整段蒸发
// （非流式早已正确）。钉住流式与非流式同款：Kind=custom、名字、InputText 原文、
// Input 投影、id 都保全。type 只在首片带、入参跨片累积都要认得。
func TestCustomToolCallStream(t *testing.T) {
	resp, _ := decodeStream(t,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[`+
			`{"index":0,"id":"call_1","type":"custom","custom":{"name":"shell","input":"echo "}}]}}]}`,
		// 后续片不再带 type，只带 custom.input：也要认作 custom 并累积。
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[`+
			`{"index":0,"custom":{"input":"hi"}}]}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		doneSentinel,
	)
	tools := toolBlocks(resp)
	if len(tools) != 1 {
		t.Fatalf("tool blocks = %d, want 1: %+v", len(tools), tools)
	}
	tu := tools[0].ToolUse
	if tu == nil {
		t.Fatalf("tool use 为 nil: %+v", tools[0])
	}
	if tu.Kind != ir.ToolCustom {
		t.Errorf("流式 custom 调用没带上 Kind=custom：%q", tu.Kind)
	}
	if tu.Name != "shell" {
		t.Errorf("名字丢失或被兜底成 unknown_tool：%q", tu.Name)
	}
	if tu.InputText != "echo hi" {
		t.Errorf("自由文本入参没跨片拼回：%q", tu.InputText)
	}
	if tu.Input != `{"input":"echo hi"}` {
		t.Errorf("Input 投影不对：%q", tu.Input)
	}
	if tu.ID != "call_1" {
		t.Errorf("id 丢失：%q", tu.ID)
	}
}

// 同一条流里 custom 与 function 两种调用并存：各走各的槽位，custom 落 InputText、
// function 落 JSON 参数，互不串味。
func TestCustomToolCallStreamMixedWithFunction(t *testing.T) {
	resp, _ := decodeStream(t,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[`+
			`{"index":0,"id":"call_c","type":"custom","custom":{"name":"shell","input":"ls -l"}}]}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[`+
			`{"index":1,"id":"call_f","type":"function","function":{"name":"get","arguments":"{\"q\":1}"}}]}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		doneSentinel,
	)
	tools := toolBlocks(resp)
	if len(tools) != 2 {
		t.Fatalf("tool blocks = %d, want 2: %+v", len(tools), tools)
	}
	byName := map[string]*ir.ToolUse{}
	for _, b := range tools {
		if b.ToolUse != nil {
			byName[b.ToolUse.Name] = b.ToolUse
		}
	}
	c := byName["shell"]
	if c == nil || c.Kind != ir.ToolCustom || c.InputText != "ls -l" || c.Input != `{"input":"ls -l"}` {
		t.Errorf("custom 调用不对：%+v", c)
	}
	f := byName["get"]
	if f == nil || f.Kind != ir.ToolFunction || f.Input != `{"q":1}` || f.InputText != "" {
		t.Errorf("function 调用被 custom 逻辑污染：%+v", f)
	}
}

// 废弃 function_call 带空名但有 arguments 载荷：非流式响应侧不得整段蒸发。
// 此前门控 Name != "" 会把「上游决定调用工具却没给名字」的调用连同 arguments
// 一起静默丢掉，而流式 announcePending 保全成 unknown_tool——同一丢弃两路分叉。
// 钉住非流式与流式同款：补 unknown_tool 占位、arguments 原样保全。
func TestDeprecatedFunctionCallEmptyNameNonStreaming(t *testing.T) {
	respBody := `{"id":"c1","model":"m","choices":[{"index":0,"finish_reason":"function_call",` +
		`"message":{"role":"assistant","function_call":{"arguments":"{\"a\":1}"}}}]}`
	resp, err := DecodeResponse([]byte(respBody))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(resp.Content) != 1 || resp.Content[0].ToolUse == nil {
		t.Fatalf("空名废弃 function_call 整段蒸发: %+v", resp.Content)
	}
	tu := resp.Content[0].ToolUse
	if tu.Name != unknownToolName {
		t.Errorf("空名没补 unknown_tool 占位：%q", tu.Name)
	}
	if tu.Input != `{"a":1}` {
		t.Errorf("arguments 载荷丢失：%q", tu.Input)
	}
}

// 请求历史侧同款：空名废弃 function_call 不得蒸发，补 unknown_tool 占位、
// arguments 保全，模型才看得到自己上一轮调用过工具。
func TestDeprecatedFunctionCallEmptyNameRequestHistory(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"assistant","function_call":{"arguments":"{\"a\":1}"}}]}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	var tu *ir.ToolUse
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockToolUse && b.ToolUse != nil {
				tu = b.ToolUse
			}
		}
	}
	if tu == nil {
		t.Fatalf("请求历史空名废弃 function_call 整段蒸发")
	}
	if tu.Name != unknownToolName {
		t.Errorf("空名没补 unknown_tool 占位：%q", tu.Name)
	}
	if tu.Input != `{"a":1}` {
		t.Errorf("arguments 载荷丢失：%q", tu.Input)
	}
}

// 流式侧回归：空名废弃 function_call 仍走 announcePending 补 unknown_tool，
// arguments 跨片拼回，收尾合成 id 并经注记报出（与非流式保全一致）。
func TestDeprecatedFunctionCallEmptyNameStream(t *testing.T) {
	resp, notes := decodeStream(t,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"function_call":{"arguments":"{\"a\":"}}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"function_call":{"arguments":"1}"}}}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"function_call"}]}`,
		doneSentinel,
	)
	tools := toolBlocks(resp)
	if len(tools) != 1 {
		t.Fatalf("tool blocks = %d, want 1: %+v", len(tools), tools)
	}
	if tools[0].ToolUse.Name != unknownToolName {
		t.Errorf("流式空名没补 unknown_tool 占位：%q", tools[0].ToolUse.Name)
	}
	if tools[0].ToolUse.Input != `{"a":1}` {
		t.Errorf("流式 arguments 未拼回：%q", tools[0].ToolUse.Input)
	}
	if !anyNoteHas(notes, "synthesized an id") {
		t.Errorf("合成 id 注记不对：%q", notes)
	}
}
