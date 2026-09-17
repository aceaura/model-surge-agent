package ir

import (
	"reflect"
	"strings"
	"testing"
)

// 这些畸形都来自真实客户端行为：上下文压缩会删掉调用只留结果、
// 会话恢复会留下悬空调用、客户端会在调用与结果之间插自己的通知消息。
// 上游对它们一律回不可重试的 400，所以必须在发出前修好。

func userText(text string) Message {
	return Message{Role: RoleUser, Content: []Block{{Type: BlockText, Text: text}}}
}

func assistantCalls(ids ...string) Message {
	m := Message{Role: RoleAssistant}
	for _, id := range ids {
		m.Content = append(m.Content, Block{
			Type:    BlockToolUse,
			ToolUse: &ToolUse{ID: id, Name: "grep", Input: `{"pattern":"x"}`},
		})
	}
	return m
}

func userResults(ids ...string) Message {
	m := Message{Role: RoleUser}
	for _, id := range ids {
		m.Content = append(m.Content, Block{
			Type: BlockToolResult,
			ToolResult: &ToolResult{
				ToolUseID: id,
				Content:   []Block{{Type: BlockText, Text: "out-" + id}},
			},
		})
	}
	return m
}

// toolIDs 收集请求里所有调用与结果的 id，用于断言配对。
func toolIDs(r *Request) (uses, results []string) {
	for _, m := range r.Messages {
		for _, b := range m.Content {
			switch b.Type {
			case BlockToolUse:
				uses = append(uses, b.ToolUse.ID)
			case BlockToolResult:
				results = append(results, b.ToolResult.ToolUseID)
			}
		}
	}
	return uses, results
}

func TestSanitizeLeavesHealthyRequestUntouched(t *testing.T) {
	// 无畸形时必须字节级不变：任何改动都会让上游的 prompt cache 前缀失配，
	// 长会话下这个代价远大于修复本身的收益。
	build := func() *Request {
		return &Request{
			Model: "m",
			Messages: []Message{
				userText("find TODO"),
				assistantCalls("call_1"),
				userResults("call_1"),
				userText("thanks"),
			},
		}
	}
	got := build()
	notes := Sanitize(got)
	if len(notes) != 0 {
		t.Fatalf("healthy request produced diagnostics: %v", notes)
	}
	if !reflect.DeepEqual(got, build()) {
		t.Fatalf("healthy request was modified\n got %#v", got.Messages)
	}
}

func TestSanitizeDemotesOrphanToolResult(t *testing.T) {
	// 上下文压缩删掉了宣告方，只剩结果。降级为文本而非丢弃：
	// 结果内容仍是模型接下来要用的事实。
	r := &Request{Messages: []Message{
		userText("hi"),
		userResults("ghost"),
	}}
	notes := Sanitize(r)
	if len(notes) == 0 {
		t.Fatal("expected a diagnostic for the orphan")
	}
	_, results := toolIDs(r)
	if len(results) != 0 {
		t.Fatalf("orphan tool_result survived: %v", results)
	}
	var found bool
	for _, m := range r.Messages {
		for _, b := range m.Content {
			if b.Type == BlockText && strings.Contains(b.Text, "[tool result for ghost]") {
				found = true
				if !strings.Contains(b.Text, "out-ghost") {
					t.Errorf("demoted text lost the original content: %q", b.Text)
				}
			}
		}
	}
	if !found {
		t.Errorf("orphan was not demoted to text: %#v", r.Messages)
	}
}

func TestSanitizeReordersDisplacedToolResult(t *testing.T) {
	// 客户端在调用与结果之间插了一条自己的通知消息。相邻判定会把这个
	// 正常配对误判成孤儿，所以配对必须走全局 id 索引。
	r := &Request{Messages: []Message{
		userText("find TODO"),
		assistantCalls("call_1"),
		userText("Approved command prefix saved"),
		userResults("call_1"),
	}}
	notes := Sanitize(r)
	if len(notes) == 0 {
		t.Fatal("expected a reorder diagnostic")
	}
	uses, results := toolIDs(r)
	if len(uses) != 1 || len(results) != 1 {
		t.Fatalf("pairing broken: uses=%v results=%v", uses, results)
	}
	// 结果必须落在宣告方的下一条消息里。
	var useMsg, resultMsg = -1, -1
	for i, m := range r.Messages {
		for _, b := range m.Content {
			if b.Type == BlockToolUse {
				useMsg = i
			}
			if b.Type == BlockToolResult {
				resultMsg = i
			}
		}
	}
	if resultMsg != useMsg+1 {
		t.Errorf("tool_result at message[%d], want message[%d]", resultMsg, useMsg+1)
	}
}

func TestSanitizeKeepsParallelResultsInCallOrder(t *testing.T) {
	// 并行调用的结果乱序到达。上游按顺序把结果绑回调用，错序会张冠李戴。
	r := &Request{Messages: []Message{
		userText("go"),
		assistantCalls("call_a", "call_b", "call_c"),
		userResults("call_c", "call_a", "call_b"),
	}}
	Sanitize(r)
	_, results := toolIDs(r)
	want := []string{"call_a", "call_b", "call_c"}
	if !reflect.DeepEqual(results, want) {
		t.Fatalf("results order = %v, want %v", results, want)
	}
}

func TestSanitizeDropsUnansweredToolUse(t *testing.T) {
	// 会话恢复截断在调用之后，结果永远不会来。上游看到「宣告了却没结果」
	// 会拒收整个请求。
	r := &Request{Messages: []Message{
		userText("go"),
		assistantCalls("answered", "dangling"),
		userResults("answered"),
	}}
	notes := Sanitize(r)
	if len(notes) == 0 {
		t.Fatal("expected a diagnostic for the dangling call")
	}
	uses, results := toolIDs(r)
	if !reflect.DeepEqual(uses, []string{"answered"}) {
		t.Errorf("uses = %v, want only the answered one", uses)
	}
	if !reflect.DeepEqual(results, []string{"answered"}) {
		t.Errorf("results = %v", results)
	}
}

func TestSanitizeDropsAssistantMessageEmptiedByToolRemoval(t *testing.T) {
	// 助手消息整条只有那个悬空调用，删掉后就空了；空消息上游同样拒收。
	r := &Request{Messages: []Message{
		userText("go"),
		assistantCalls("dangling"),
	}}
	Sanitize(r)
	for _, m := range r.Messages {
		if m.Role == RoleAssistant {
			t.Fatalf("emptied assistant message survived: %#v", m)
		}
	}
	if len(r.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(r.Messages))
	}
}

func TestSanitizeKeepsOnlyLastDuplicateToolResult(t *testing.T) {
	// 重连会让上游重放同一个结果。留最后一份：它是重连后的权威值。
	r := &Request{Messages: []Message{
		userText("go"),
		assistantCalls("call_1"),
		userResults("call_1"),
		userResults("call_1"),
	}}
	notes := Sanitize(r)
	if len(notes) == 0 {
		t.Fatal("expected a duplicate diagnostic")
	}
	_, results := toolIDs(r)
	if len(results) != 1 {
		t.Fatalf("kept %d duplicate results, want 1", len(results))
	}
}

func TestSanitizeDropsEmptyMessages(t *testing.T) {
	r := &Request{Messages: []Message{
		userText("hi"),
		{Role: RoleUser, Content: []Block{{Type: BlockText, Text: "   "}}},
		{Role: RoleUser},
	}}
	notes := Sanitize(r)
	if len(notes) != 2 {
		t.Fatalf("diagnostics = %v, want 2", notes)
	}
	if len(r.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(r.Messages))
	}
}

func TestSanitizeTrimsAssistantPrefillWhitespace(t *testing.T) {
	// Anthropic 拒收以空白结尾的助手 prefill。
	r := &Request{Messages: []Message{
		userText("hi"),
		{Role: RoleAssistant, Content: []Block{{Type: BlockText, Text: "Sure thing\n\n  "}}},
	}}
	notes := Sanitize(r)
	if len(notes) == 0 {
		t.Fatal("expected a trim diagnostic")
	}
	got := r.Messages[1].Content[0].Text
	if got != "Sure thing" {
		t.Errorf("prefill = %q, want %q", got, "Sure thing")
	}
}

func TestSanitizeDropsWhitespaceOnlyAssistantPrefill(t *testing.T) {
	// trim 后整条变空，应当整条删掉而不是留一个空块。
	r := &Request{Messages: []Message{
		userText("hi"),
		{Role: RoleAssistant, Content: []Block{{Type: BlockText, Text: "   \n"}}},
	}}
	Sanitize(r)
	if len(r.Messages) != 1 || r.Messages[0].Role != RoleUser {
		t.Fatalf("messages = %#v, want only the user turn", r.Messages)
	}
}

func TestSanitizeHandlesNilAndEmpty(t *testing.T) {
	if notes := Sanitize(nil); notes != nil {
		t.Errorf("Sanitize(nil) = %v", notes)
	}
	if notes := Sanitize(&Request{}); notes != nil {
		t.Errorf("Sanitize(empty) = %v", notes)
	}
}
