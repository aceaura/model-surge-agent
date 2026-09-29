package gemini

import (
	"encoding/json"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次72：证明系统提示引用注记对应一次真实的编码丢弃——gemini 把 req.System 经
// joinText 收敛成 systemInstruction.parts[].text，而 wirePart 根本没有引用字段
// （引用槽 citationMetadata/groundingMetadata 仅在候选输出侧），系统提示文本块上的
// 引用整组消失。注记若不报，就是静默丢弃。
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
	if w.SystemInstruction == nil || len(w.SystemInstruction.Parts) != 1 {
		t.Fatalf("want a single-part systemInstruction, got %+v (%s)", w.SystemInstruction, raw)
	}
	// part 只装 Text：wirePart 无引用槽，URL/offsets 全部消失。
	if got := w.SystemInstruction.Parts[0].Text; got != "北京" {
		t.Errorf("systemInstruction part text 应是纯文本 %q，实得 %q：%s", "北京", got, raw)
	}
}
