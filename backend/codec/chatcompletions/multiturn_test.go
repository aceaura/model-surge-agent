package chatcompletions

import "testing"

// 两轮响应各自的解码器都从序号 1 开始。没有 scope 时同名调用会拿到
// 同一个合成 id，客户端把两轮都回传进历史后配对就错位到另一轮的调用上。
// 用上游响应 id 作 scope 后，两轮必须给出不同的 id。
func TestSynthIDsDifferAcrossTurns(t *testing.T) {
	first := onlyToolID(t, "resp-1")
	second := onlyToolID(t, "resp-2")
	if first == second {
		t.Fatalf("两轮的无 id 调用撞成同一个合成 id %q", first)
	}
}

// 同一次响应内的两个无 id 调用仍必须互不相同：
// scope 相同，只有序号能区分它们。
func TestSynthIDsDifferWithinOneTurn(t *testing.T) {
	resp, _ := decodeStream(t,
		`{"id":"resp-1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[`+
			`{"index":0,"type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`,
		`{"id":"resp-1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[`+
			`{"index":1,"type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`,
		doneSentinel,
	)
	tools := toolBlocks(resp)
	if len(tools) != 2 {
		t.Fatalf("tool blocks = %d, want 2", len(tools))
	}
	if tools[0].ToolUse.ID == tools[1].ToolUse.ID {
		t.Fatalf("同轮内两次调用撞 id %q", tools[0].ToolUse.ID)
	}
}

// 上游给了真 id 时一律原样保留：改写会让上游对不上自己宣告的调用。
func TestUpstreamToolIDSurvivesScoping(t *testing.T) {
	resp, _ := decodeStream(t,
		`{"id":"resp-1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[`+
			`{"index":0,"id":"call_real","type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`,
		doneSentinel,
	)
	tools := toolBlocks(resp)
	if len(tools) != 1 || tools[0].ToolUse.ID != "call_real" {
		t.Fatalf("上游 id 未原样保留：%+v", tools)
	}
}

// onlyToolID 解一轮只含单个无 id 工具调用的流，返回它的合成 id。
func onlyToolID(t *testing.T, respID string) string {
	t.Helper()
	resp, _ := decodeStream(t,
		`{"id":"`+respID+`","model":"m","choices":[{"index":0,"delta":{"tool_calls":[`+
			`{"index":0,"type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`,
		doneSentinel,
	)
	tools := toolBlocks(resp)
	if len(tools) != 1 {
		t.Fatalf("tool blocks = %d, want 1", len(tools))
	}
	return tools[0].ToolUse.ID
}
