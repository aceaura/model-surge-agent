package chatcompletions

import (
	"encoding/json"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次72：证明 DescribeLossy 的系统提示引用注记对应一次真实的编码丢弃——chat
// 请求编码器把 req.System 经 encodeContent 收敛成纯文本 system 消息（annotations
// 仅在 assistant 消息写，system 消息根本不经 encodeMessage），系统提示文本块上的
// 引用根本不出现在 wire 上。注记若不报，就是静默丢弃。
func TestR72EncodeRequestDropsSystemAnnotations(t *testing.T) {
	cite := ir.Citation{URL: "https://wx.test/1", CitedText: "北京", Start: 0, End: 2}
	req := &ir.Request{Model: "m",
		System: []ir.Block{{Type: ir.BlockText, Text: "北京", Citations: []ir.Citation{cite}}},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
		},
	}
	raw, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var w wireRequest
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatal(err)
	}
	if len(w.Messages) == 0 {
		t.Fatalf("want a leading system message, got none (%s)", raw)
	}
	sys := w.Messages[0]
	if sys.Role != roleSystem {
		t.Fatalf("want first message role %q, got %q (%s)", roleSystem, sys.Role, raw)
	}
	// 系统提示引用被丢：system 消息无 annotations，content 收敛成纯文本字符串。
	if n := len(sys.Annotations); n != 0 {
		t.Errorf("系统提示引用应被丢弃，却写出 %d 条 annotations：%s", n, raw)
	}
	var content string
	if err := json.Unmarshal(sys.Content, &content); err != nil {
		t.Fatalf("system content 应是纯文本字符串，解析失败：%v (%s)", err, raw)
	}
	if content != "北京" {
		t.Errorf("system content 应是纯文本 %q，实得 %q（引用未被丢弃？）：%s", "北京", content, raw)
	}
}
