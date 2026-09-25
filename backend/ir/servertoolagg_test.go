package ir

import "testing"

// 托管工具调用的查询串走 EvToolInput 通道，聚合器必须按块型归位到
// ServerToolUse.Input——写进 ToolUse 槽位会让同族回吐与外族注记都看错字段。
func TestServerToolInputAccumulatesViaToolInputChannel(t *testing.T) {
	a := &Aggregator{}
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{
		Type: BlockServerToolUse,
		ServerToolUse: &ServerToolUse{
			ID: "srvtoolu_1", Name: "web_search",
		},
	}})
	a.Add(Event{Type: EvToolInput, Index: 0, Text: `{"query":`})
	a.Add(Event{Type: EvToolInput, Index: 0, Text: `"weather in Paris"}`})
	a.Add(Event{Type: EvBlockStop, Index: 0})

	got := a.Response()
	if len(got.Content) != 1 {
		t.Fatalf("content = %#v", got.Content)
	}
	b := got.Content[0]
	if b.Type != BlockServerToolUse {
		t.Fatalf("block type = %q", b.Type)
	}
	if b.ToolUse != nil {
		t.Errorf("查询串被写进了 ToolUse 槽位: %#v", b.ToolUse)
	}
	stu := b.ServerToolUse
	if stu == nil || stu.ID != "srvtoolu_1" || stu.Name != "web_search" {
		t.Fatalf("server tool use = %#v", stu)
	}
	if stu.Input != `{"query":"weather in Paris"}` {
		t.Errorf("input = %q", stu.Input)
	}
}

// 上游在查询串发到一半断流：已收片段必须留在 ServerToolUse.Input 里
// （不能凭空清空，也不能补全），供落库与诊断看到真实截断形态。
func TestServerToolInputSurvivesTruncatedStream(t *testing.T) {
	a := &Aggregator{}
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{
		Type:          BlockServerToolUse,
		ServerToolUse: &ServerToolUse{ID: "srvtoolu_1", Name: "web_search"},
	}})
	a.Add(Event{Type: EvToolInput, Index: 0, Text: `{"query":"wea`})

	got := a.Response()
	if len(got.Content) != 1 || got.Content[0].ServerToolUse == nil {
		t.Fatalf("content = %#v", got.Content)
	}
	if in := got.Content[0].ServerToolUse.Input; in != `{"query":"wea` {
		t.Errorf("input = %q", in)
	}
	// 截断的托管工具调用不欠客户端结果，IncompleteTools 只管 BlockToolUse。
	if inc := a.IncompleteTools(); len(inc) != 0 {
		t.Errorf("IncompleteTools = %v, 托管工具不该出现在里面", inc)
	}
}

// 块开始事件自带完整查询串时，seed 记账、materialize 赋值回写：
// 同一段内容不能被数出两份。
func TestSeededServerToolInputNotDoubled(t *testing.T) {
	a := &Aggregator{}
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{
		Type: BlockServerToolUse,
		ServerToolUse: &ServerToolUse{
			ID: "srvtoolu_1", Name: "web_search",
			Input: `{"query":"seeded"}`,
		},
	}})
	a.Add(Event{Type: EvBlockStop, Index: 0})

	// 调两次 Response：materialize 必须幂等。
	a.Response()
	got := a.Response()
	if len(got.Content) != 1 || got.Content[0].ServerToolUse == nil {
		t.Fatalf("content = %#v", got.Content)
	}
	if in := got.Content[0].ServerToolUse.Input; in != `{"query":"seeded"}` {
		t.Errorf("input = %q, 种子内容被翻倍或抹掉", in)
	}
}

// web_search_tool_result 块自带完整载荷（不走增量通道），聚合器原样保留，
// 且不能给它伪造 ServerToolUse 槽位。
func TestWebSearchResultBlockPassesThroughAggregator(t *testing.T) {
	res := &WebSearchToolResult{
		ToolUseID: "srvtoolu_1",
		Results: []WebSearchResult{
			{Title: "Paris weather", URL: "https://example.com/p",
				Snippet: "enc", PageAge: "2026"},
		},
	}
	a := &Aggregator{}
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{
		Type: BlockWebSearchToolResult, WebSearchToolResult: res,
	}})
	a.Add(Event{Type: EvBlockStop, Index: 0})

	got := a.Response()
	if len(got.Content) != 1 {
		t.Fatalf("content = %#v", got.Content)
	}
	b := got.Content[0]
	if b.Type != BlockWebSearchToolResult {
		t.Fatalf("block type = %q", b.Type)
	}
	if b.WebSearchToolResult == nil ||
		len(b.WebSearchToolResult.Results) != 1 ||
		b.WebSearchToolResult.Results[0].URL != "https://example.com/p" {
		t.Errorf("result = %#v", b.WebSearchToolResult)
	}
	if b.ServerToolUse != nil {
		t.Errorf("凭空多出 ServerToolUse: %#v", b.ServerToolUse)
	}
}

// cloneBlocks 必须深拷贝 Results 切片：改副本不能穿透到原件。
func TestCloneBlocksDeepCopiesWebSearchResults(t *testing.T) {
	src := []Block{{
		Type: BlockWebSearchToolResult,
		WebSearchToolResult: &WebSearchToolResult{
			ToolUseID: "srvtoolu_1",
			Results:   []WebSearchResult{{Title: "a", URL: "u"}},
		},
		ServerToolUse: &ServerToolUse{ID: "s", Name: "n", Input: "i"},
	}}
	dst := cloneBlocks(src)
	dst[0].WebSearchToolResult.Results[0].Title = "mutated"
	dst[0].WebSearchToolResult.ToolUseID = "other"
	dst[0].ServerToolUse.Input = "other"
	if src[0].WebSearchToolResult.Results[0].Title != "a" ||
		src[0].WebSearchToolResult.ToolUseID != "srvtoolu_1" ||
		src[0].ServerToolUse.Input != "i" {
		t.Errorf("clone 穿透改到了原件: %#v", src[0])
	}
}
