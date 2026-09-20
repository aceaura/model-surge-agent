package ir

import (
	"reflect"
	"testing"
)

// 相邻同角色消息在 Chat Completions 里合法，Anthropic 却拒收——
// 后者的 400 不可重试，换目标也救不回来，只能在发出前合并。

func TestAdjacentSameRoleMessagesMerged(t *testing.T) {
	r := &Request{
		Model: "m",
		Messages: []Message{
			userText("first"),
			userText("second"),
			assistantText("ok"),
		},
	}
	notes := Sanitize(r)
	if len(r.Messages) != 2 {
		t.Fatalf("应合并成 2 条，got %d：%#v", len(r.Messages), r.Messages)
	}
	if r.Messages[0].Role != RoleUser || r.Messages[1].Role != RoleAssistant {
		t.Fatalf("合并后角色错乱：%#v", r.Messages)
	}
	// 不把两个文本块并成一个（那会让 CacheCtl 这类块级属性无处安放），
	// 但边界处插一个空行分隔块：出站编码器的文本拼接是无分隔的，
	// 不插会让「first」「second」在 responses / gemini / chat_completions
	// 的字符串形态里粘成「firstsecond」。
	want := []string{"first", "\n\n", "second"}
	got := r.Messages[0].Content
	if len(got) != len(want) {
		t.Fatalf("块数 = %d，想要 %d（文本、分隔、文本）：%#v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Type != BlockText || got[i].Text != w {
			t.Errorf("块 %d = %+v，想要文本 %q", i, got[i], w)
		}
	}
	if !hasNote(notes, "merged 1 adjacent same-role message") {
		t.Errorf("notes = %v，合并必须出说明：客户端需要知道「我发了 3 条、上游看到 2 条」不是丢消息", notes)
	}
}

// TestAlternatingHistoryUntouched 未触发路径：已交替的历史字节不变、
// 不出说明。合并的说明若无条件产出，每个正常请求都会带一条噪声。
func TestAlternatingHistoryUntouched(t *testing.T) {
	build := func() *Request {
		return &Request{
			Model: "m",
			Messages: []Message{
				userText("a"),
				assistantText("b"),
				userText("c"),
			},
		}
	}
	got := build()
	notes := Sanitize(got)
	if len(notes) != 0 {
		t.Fatalf("已交替的历史产出了说明：%v", notes)
	}
	if !reflect.DeepEqual(got, build()) {
		t.Fatalf("已交替的历史被改动了：%#v", got.Messages)
	}
}

// TestMergeAfterEmptyPruneCatchesNewAdjacency 顺序判据：丢掉中间那条空消息
// 会让原本被隔开的两条 user 变成相邻。合并排在清空之前就会漏掉这一形态。
func TestMergeAfterEmptyPruneCatchesNewAdjacency(t *testing.T) {
	r := &Request{
		Model: "m",
		Messages: []Message{
			userText("first"),
			{Role: RoleAssistant, Content: []Block{{Type: BlockText, Text: "   "}}},
			userText("second"),
		},
	}
	notes := Sanitize(r)
	if len(r.Messages) != 1 {
		t.Fatalf("清空后产生的相邻未被合并，got %d 条：%#v", len(r.Messages), r.Messages)
	}
	if r.Messages[0].Role != RoleUser {
		t.Fatalf("合并后角色错了：%#v", r.Messages)
	}
	if !hasNote(notes, "dropped empty assistant") {
		t.Errorf("notes = %v，缺少丢弃空消息的说明", notes)
	}
	if !hasNote(notes, "merged 1 adjacent same-role message") {
		t.Errorf("notes = %v，缺少合并说明", notes)
	}
}

// TestMergeCountsEveryCollapse 说明里的条数要如实：三条连续 user 合成一条
// 是合并了两次，报 1 会让读者以为只少了一条。
func TestMergeCountsEveryCollapse(t *testing.T) {
	r := &Request{
		Model: "m",
		Messages: []Message{
			userText("a"), userText("b"), userText("c"),
			assistantText("d"),
			userText("e"), userText("f"),
		},
	}
	notes := Sanitize(r)
	if len(r.Messages) != 3 {
		t.Fatalf("应合并成 3 条，got %d：%#v", len(r.Messages), r.Messages)
	}
	if !hasNote(notes, "merged 3 adjacent same-role message") {
		t.Errorf("notes = %v，合并了 3 次就要报 3", notes)
	}
}

// TestMergeDoesNotAliasInputSlices 合并不得写进入参消息的富余容量：
// 同一份请求可能被复用，串写会让第二次看到多出来的块。
func TestMergeDoesNotAliasInputSlices(t *testing.T) {
	shared := make([]Block, 1, 4)
	shared[0] = Block{Type: BlockText, Text: "first"}
	r := &Request{
		Model: "m",
		Messages: []Message{
			{Role: RoleUser, Content: shared},
			userText("second"),
			assistantText("ok"),
		},
	}
	Sanitize(r)
	if len(shared) != 1 || shared[0].Text != "first" {
		t.Fatalf("入参切片被改动了：%#v", shared)
	}
	if cap(shared) >= 2 && shared[:2][1].Text == "second" {
		t.Errorf("合并写进了入参的富余容量")
	}
}

// TestSingleMessageNotMerged 边界：只有一条消息时不进合并逻辑。
func TestSingleMessageNotMerged(t *testing.T) {
	r := &Request{Model: "m", Messages: []Message{userText("only")}}
	if notes := Sanitize(r); len(notes) != 0 {
		t.Errorf("单条消息产出了说明：%v", notes)
	}
	if len(r.Messages) != 1 {
		t.Errorf("单条消息数量变了：%#v", r.Messages)
	}
}
