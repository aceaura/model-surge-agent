package ir

import (
	"strings"
	"testing"
)

// declRequest 是一份带工具声明与一条 tool_use 历史的最小请求。
func declRequest(tools []Tool, useName string) *Request {
	req := &Request{
		Tools: tools,
		Messages: []Message{
			{Role: RoleUser, Content: []Block{{Type: BlockText, Text: "hi"}}},
		},
	}
	if useName != "" {
		req.Messages = append(req.Messages, Message{Role: RoleAssistant, Content: []Block{
			{Type: BlockToolUse, ToolUse: &ToolUse{ID: "call_1", Name: useName, Input: "{}"}},
		}})
		req.Messages = append(req.Messages, Message{Role: RoleUser, Content: []Block{
			{Type: BlockToolResult, ToolResult: &ToolResult{ToolUseID: "call_1",
				Content: []Block{{Type: BlockText, Text: "done"}}}},
		}})
	}
	return req
}

func TestGovernToolDeclsLeavesCleanRequestAlone(t *testing.T) {
	req := declRequest([]Tool{{Name: "grep", Schema: `{"type":"object"}`}}, "grep")
	notes := Sanitize(req)
	if len(notes) != 0 {
		t.Fatalf("合法声明不该有说明，实得 %v", notes)
	}
	if req.Tools[0].Name != "grep" {
		t.Fatalf("合法名字被改写成 %q", req.Tools[0].Name)
	}
}

func TestGovernToolDeclsDropsEmptyName(t *testing.T) {
	req := declRequest([]Tool{{Name: "  "}, {Name: "grep"}}, "")
	notes := Sanitize(req)
	if len(req.Tools) != 1 || req.Tools[0].Name != "grep" {
		t.Fatalf("空名声明未被丢弃，实得 %+v", req.Tools)
	}
	if !hasNote(notes, "empty name") {
		t.Fatalf("未报空名说明：%v", notes)
	}
}

func TestGovernToolDeclsDropsDuplicates(t *testing.T) {
	req := declRequest([]Tool{
		{Name: "grep", Description: "first"},
		{Name: "grep", Description: "second"},
	}, "")
	notes := Sanitize(req)
	if len(req.Tools) != 1 {
		t.Fatalf("重名声明未去重，实得 %d 条", len(req.Tools))
	}
	if req.Tools[0].Description != "first" {
		t.Fatalf("去重应保留首个，实得 %q", req.Tools[0].Description)
	}
	if !hasNote(notes, "duplicate") || !hasNote(notes, "grep") {
		t.Fatalf("未报点名的重名说明：%v", notes)
	}
}

func TestGovernToolDeclsReplacesIllegalChars(t *testing.T) {
	req := declRequest([]Tool{{Name: "my tool:read!"}}, "my tool:read!")
	notes := Sanitize(req)
	if req.Tools[0].Name != "my_tool_read_" {
		t.Fatalf("非法字符未替换，实得 %q", req.Tools[0].Name)
	}
	if !hasNote(notes, "rewrote tool name") {
		t.Fatalf("未报改写说明：%v", notes)
	}
	// 历史里的 tool_use 必须跟着改，否则模型下一轮还照旧名字调。
	got := req.Messages[1].Content[0].ToolUse.Name
	if got != "my_tool_read_" {
		t.Fatalf("历史 tool_use 未同步改写，实得 %q", got)
	}
}

func TestGovernToolDeclsTruncatesWithoutCollision(t *testing.T) {
	// 两个名字前 59 字符完全相同：纯截断会撞成同名，撞名后模型调用
	// 哪个都是错的，且没有任何症状可查。
	prefix := strings.Repeat("a", 60)
	req := declRequest([]Tool{
		{Name: prefix + "_variant_a"},
		{Name: prefix + "_variant_b"},
	}, "")
	notes := Sanitize(req)
	if len(req.Tools) != 2 {
		t.Fatalf("截断后撞名致声明被去重，实得 %d 条：%v", len(req.Tools), notes)
	}
	for _, tool := range req.Tools {
		if len(tool.Name) != maxToolNameLen {
			t.Fatalf("截断后长度应恰为 %d，实得 %d (%q)", maxToolNameLen, len(tool.Name), tool.Name)
		}
	}
	if req.Tools[0].Name == req.Tools[1].Name {
		t.Fatalf("两个长名截断后撞名：%q", req.Tools[0].Name)
	}
}

func TestGovernToolDeclsKeepsPairingIntact(t *testing.T) {
	req := declRequest([]Tool{{Name: "read file"}}, "read file")
	notes := Sanitize(req)
	// 改写名字不该把配对判成孤儿——配对靠 id，不靠名字。
	for _, n := range notes {
		if strings.Contains(n, "orphan") || strings.Contains(n, "demoted") {
			t.Fatalf("改写工具名后配对被误判：%v", notes)
		}
	}
	if req.Messages[2].Content[0].Type != BlockToolResult {
		t.Fatalf("工具结果被降级，实得 %+v", req.Messages[2].Content[0])
	}
}

func TestGovernToolDeclsSkippedWithoutTools(t *testing.T) {
	req := declRequest(nil, "")
	if notes := Sanitize(req); len(notes) != 0 {
		t.Fatalf("无工具声明不该有说明，实得 %v", notes)
	}
}

func hasNote(notes []string, want string) bool {
	for _, n := range notes {
		if strings.Contains(n, want) {
			return true
		}
	}
	return false
}
