package chatcompletions

import (
	"encoding/json"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次71：证明 DescribeLossy 的 stray 注记对应一次真实的编码丢弃——chat 请求
// 编码器只在 assistant 消息写 annotations（encode_request.go 的 role 门控），
// user 消息上的可移植引用根本不出现在 wire 上。注记若不报，就是静默丢弃。
func TestR71EncodeRequestDropsUserRoleAnnotations(t *testing.T) {
	cite := ir.Citation{URL: "https://wx.test/1", CitedText: "北京", Start: 0, End: 2}
	req := &ir.Request{Model: "m", Messages: []ir.Message{
		// user 角色：可移植引用应被丢弃（wire 上无 annotations）。
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "北京", Citations: []ir.Citation{cite}}}},
		// assistant 角色：同款引用应保留（对照组，证明门控是按角色而非全局）。
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: "北京", Citations: []ir.Citation{cite}}}},
	}}
	raw, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var w wireRequest
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatal(err)
	}
	if len(w.Messages) != 2 {
		t.Fatalf("want 2 messages, got %d (%s)", len(w.Messages), raw)
	}
	if n := len(w.Messages[0].Annotations); n != 0 {
		t.Errorf("user 角色引用应被丢弃，却写出 %d 条 annotations：%s", n, raw)
	}
	if n := len(w.Messages[1].Annotations); n != 1 {
		t.Errorf("assistant 角色引用应保留 1 条 annotations，实得 %d：%s", n, raw)
	}
}
