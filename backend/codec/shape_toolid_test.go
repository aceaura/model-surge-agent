package codec

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 能力位为零时整段跳过：四协议当前都是零值，没有实测证据的上限是猜的，
// 按猜的值改 id 只会把本来能过的请求改坏。
func TestShapeToolIDsSkipsWhenNoLimit(t *testing.T) {
	long := strings.Repeat("x", 200)
	req := reqWithToolPair(long)
	c := &noteCollector{name: "probe"}

	shapeToolIDs(req, Capabilities{}, c)

	if got := toolUseID(req); got != long {
		t.Errorf("上限为零仍改了 id：%q", got)
	}
	if notes := c.notes(); len(notes) != 0 {
		t.Errorf("上限为零不该有说明，实得 %v", notes)
	}
}

// 未超限的 id 原样透传且不报说明：恒定出现的说明会淹掉真正的有损信号。
func TestShapeToolIDsLeavesShortIDsAlone(t *testing.T) {
	req := reqWithToolPair("call_short")
	c := &noteCollector{name: "probe"}

	shapeToolIDs(req, Capabilities{MaxToolIDLen: 32}, c)

	if got := toolUseID(req); got != "call_short" {
		t.Errorf("未超限的 id 被改成了 %q", got)
	}
	if notes := c.notes(); len(notes) != 0 {
		t.Errorf("未超限不该有说明，实得 %v", notes)
	}
}

// 超限的 id 要收敛，并且必须同步改写结果侧的配对键。
// 只改一侧会让上游拿到一个指向不存在调用的结果，整轮请求被拒。
func TestShapeToolIDsShortensAndKeepsPairing(t *testing.T) {
	long := "msa_synth_" + strings.Repeat("n", 80) + "_1"
	req := reqWithToolPair(long)
	c := &noteCollector{name: "probe"}

	shapeToolIDs(req, Capabilities{MaxToolIDLen: 24}, c)

	use := toolUseID(req)
	result := toolResultID(req)
	if len(use) > 24 {
		t.Errorf("收敛后仍有 %d 字节：%q", len(use), use)
	}
	if use == long {
		t.Fatalf("超限的 id 未被收敛：%q", use)
	}
	if use != result {
		t.Errorf("配对断开：tool_use=%q 而 tool_result=%q", use, result)
	}
	notes := c.notes()
	if len(notes) != 1 || !strings.Contains(notes[0], "tool call id") {
		t.Errorf("改写 id 必须报一条说明（客户端下一轮回传的 id 将与上一轮不一致），实得 %v", notes)
	}
}

// 两个不同的原始 id 收敛后不得相同：撞号等于把两次调用并成一次，
// 客户端会把两份入参串成一份非法 JSON。
func TestShapeToolIDsAvoidsCollision(t *testing.T) {
	const max = 20
	long := strings.Repeat("p", 40) + "aaa"
	// 撞车成色：另一次调用的 id 本身未超限，却恰好等于长 id 的收敛结果。
	// 两个长 id 之间撞不上（哈希不同），能撞上的是「收敛结果」与「既有短 id」，
	// 所以这里直接取收敛结果当第二个调用的 id。
	occupied := shortenToolID(long, max, 0)

	req := &ir.Request{Messages: []ir.Message{
		{Role: ir.RoleAssistant, Content: []ir.Block{
			{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: long, Name: "grep"}},
			{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: occupied, Name: "read"}},
		}},
		{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{ToolUseID: long}},
			{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{ToolUseID: occupied}},
		}},
	}}
	c := &noteCollector{name: "probe"}

	shapeToolIDs(req, Capabilities{MaxToolIDLen: max}, c)

	uses := req.Messages[0].Content
	got1, got2 := uses[0].ToolUse.ID, uses[1].ToolUse.ID
	if got1 == got2 {
		t.Fatalf("长 id 收敛后撞上了既有的 %q，两次调用并成了一次", got1)
	}
	// 撞车必须靠重取解决，而不是放弃收敛：放弃会把一个超限的 id 发给上游。
	if len(got1) > max {
		t.Errorf("撞车后放弃了收敛，长 id 仍有 %d 字节：%q", len(got1), got1)
	}
	if got2 != occupied {
		t.Errorf("未超限的 id 不该被动：%q → %q", occupied, got2)
	}
	// 各自的结果必须跟着各自的调用走。
	results := req.Messages[1].Content
	if results[0].ToolResult.ToolUseID != got1 {
		t.Errorf("第一个结果配到了 %q，应为 %q", results[0].ToolResult.ToolUseID, got1)
	}
	if results[1].ToolResult.ToolUseID != got2 {
		t.Errorf("第二个结果配到了 %q，应为 %q", results[1].ToolResult.ToolUseID, got2)
	}
}

// 收敛后仍要能判出这是合成 id：前缀承载省略与否的判定，丢了它
// 出站侧就无从决定该不该把 id 写进请求体。
func TestShortenedSynthIDKeepsItsPrefix(t *testing.T) {
	long := SynthToolID("resp-1", strings.Repeat("n", 80), 1)
	req := reqWithToolPair(long)

	shapeToolIDs(req, Capabilities{MaxToolIDLen: 32}, &noteCollector{name: "probe"})

	if got := toolUseID(req); !IsSynthToolID(got) {
		t.Errorf("收敛后前缀丢了，判不出是合成 id：%q", got)
	}
}

func reqWithToolPair(id string) *ir.Request {
	return &ir.Request{Messages: []ir.Message{
		{Role: ir.RoleAssistant, Content: []ir.Block{
			{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: id, Name: "grep"}},
		}},
		{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{ToolUseID: id}},
		}},
	}}
}

func toolUseID(req *ir.Request) string {
	return req.Messages[0].Content[0].ToolUse.ID
}

func toolResultID(req *ir.Request) string {
	return req.Messages[1].Content[0].ToolResult.ToolUseID
}
