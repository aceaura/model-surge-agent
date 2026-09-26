package gemini

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// safetySettings 键不得出现：wireRequest 曾经留着这个槽位，但 IR 没有安全
// 阈值维度、入站三协议也没有对应参数，它是一个永远为空的死键。删掉之后
// 用本测试钉住——将来谁要重新引入安全设置，必须先给 IR 加维度并想清楚
// 跨协议投影，而不是在线体上悄悄多写一个键。
func TestOutboundRequestNeverWritesSafetySettings(t *testing.T) {
	req := &ir.Request{
		Model: "gemini-2.5-pro",
		Messages: []ir.Message{{
			Role:    ir.RoleUser,
			Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}},
		}},
	}
	body, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if strings.Contains(string(body), "safetySettings") {
		t.Fatalf("出站请求体不得出现 safetySettings 死键: %s", body)
	}
}
