package anthropic

import (
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次82：shape 把 thinking 整块丢掉之后，诊断侧不得再报「合成了思考预算」。
//
// 出站编码刻意按「原始请求」推导有损说明（见 anthropic/codec.go 的
// EncodeRequestLossy：shape 跑在副本上，DescribeLossy 跑在原件上，以免漏报被
// shape 降级掉的字段）。副作用是 lossy.go 的 effort→budget 合成注记只按原始
// req 的 Thinking.On()&&BudgetTokens<=0 触发，看不到 shape 已把 thinking 丢弃，
// 于是在「思考已被丢掉」时仍报「合成了一个预算」——与 shapeNotes 里的
// "dropped thinking" 自相矛盾的假阳性（违规则 a：注记必须与真实处置相符）。
// 修复用 codec.thinkingSurvivesShaping 门控那条注记。
//
// 三例钉住：两条丢弃路径（强制工具、max_tokens 容不下最小预算）不再报合成，
// 而思考确实存活到编码那一步时合成注记照报（防门控过度抑制）。
//
// 关键：必须 BudgetTokens<=0 才会走到合成注记那支。既有的
// TestForcedToolChoiceDisablesThinking 用 BudgetTokens:4096 恰好绕开了它，
// 所以这个假阳性此前没被任何测试触到——不是有意行为，是盲区。

// 强制工具调用关掉思考时，不得再报「合成了思考预算」。
func TestShapedOutThinkingByForcedToolNotReportedAsFilled(t *testing.T) {
	req := baseRequest() // MaxTokens=8192 预算充足，丢弃只因强制工具互斥
	req.Tools = []ir.Tool{{Name: "read", Schema: `{"type":"object","properties":{}}`}}
	req.ToolChoice = &ir.ToolChoice{Mode: ir.ToolChoiceTool, Name: "read"}
	// 给了档位、BudgetTokens=0：正是 lossy.go effort→budget 合成支的输入形状。
	req.Thinking = &ir.ThinkingConfig{Enabled: ir.ThinkingOn(), Effort: "high"}

	obj, notes := shapedBody(t, req)
	if _, ok := obj["thinking"]; ok {
		t.Fatalf("强制工具时思考该被整块关掉：%v", obj["thinking"])
	}
	if !hasNote(notes, "forced tool choice") {
		t.Errorf("应报思考因强制工具被丢：%v", notes)
	}
	if hasNote(notes, "filled in thinking budget") {
		t.Errorf("思考已被丢弃，不该再报合成预算（假阳性）：%v", notes)
	}
}

// max_tokens 容不下协议最小推理预算而关掉思考时，同样不得报合成。
func TestShapedOutThinkingByMinBudgetNotReportedAsFilled(t *testing.T) {
	req := baseRequest()
	req.MaxTokens = 512 // effMax-1=511 < MinThinkingBudget(1024) → shape 丢弃思考
	req.Thinking = &ir.ThinkingConfig{Enabled: ir.ThinkingOn(), Effort: "high"}

	obj, notes := shapedBody(t, req)
	if _, ok := obj["thinking"]; ok {
		t.Fatalf("max_tokens 过小，思考该被关掉：%v", obj["thinking"])
	}
	if !hasNote(notes, "minimum reasoning budget") {
		t.Errorf("应报思考因预算下限被丢：%v", notes)
	}
	if hasNote(notes, "filled in thinking budget") {
		t.Errorf("思考已被丢弃，不该再报合成预算（假阳性）：%v", notes)
	}
}

// 反例守卫：思考确实活到编码那一步时，缺预算仍要报合成——门控不得过度抑制，
// 否则会把真阳一起杀掉，退化成「永不报合成预算」的漏报。
func TestSurvivingThinkingStillReportsFilledBudget(t *testing.T) {
	req := baseRequest() // MaxTokens=8192 充足、无强制工具、无采样参数 → 思考存活
	req.Thinking = &ir.ThinkingConfig{Enabled: ir.ThinkingOn(), Effort: "high"}

	obj, notes := shapedBody(t, req)
	if _, ok := obj["thinking"]; !ok {
		t.Fatalf("无丢弃触发，思考该保留并合成预算：%v", obj)
	}
	if !hasNote(notes, "filled in thinking budget") {
		t.Errorf("思考存活且缺预算，应报合成预算：%v", notes)
	}
}
