package responses

import (
	"encoding/json"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次72：证明系统提示引用注记对应一次真实的编码丢弃——responses 把 req.System
// 经 joinText 收敛成字符串 instructions（只取 BlockText.Text），系统提示文本块上的
// 引用既无 URL 也无 offsets 结构可依附，整组消失。注记若不报，就是静默丢弃。
func TestR72EncodeRequestDropsSystemCitations(t *testing.T) {
	cite := ir.Citation{URL: "https://wx.test/1", CitedText: "北京", Start: 0, End: 2}
	req := &ir.Request{Model: "m",
		System: []ir.Block{{Type: ir.BlockText, Text: "北京", Citations: []ir.Citation{cite}}},
	}
	raw, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var w wireRequest
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatal(err)
	}
	// instructions 是纯字符串：只剩正文文本，引用的 URL/offsets 全部丢弃。
	if w.Instructions != "北京" {
		t.Errorf("instructions 应是纯文本 %q，实得 %q（引用未被丢弃？）：%s", "北京", w.Instructions, raw)
	}
}
