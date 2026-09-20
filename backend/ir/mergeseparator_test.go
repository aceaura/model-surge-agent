package ir

import "testing"

// 这个文件守的是合并边界的分隔符。出站编码器的文本拼接是无分隔的
// （responses 的 instructions、gemini 的 systemInstruction、chat_completions
// 的字符串形态 content 都是直接相加），而 mergeAdjacentRoles 制造了相邻的
// 两个文本块。不插分隔符时「订单号 10086」与「3 件退货」会粘成
// 「订单号 100863 件退货」——模型算错、答错，全程 200 且无任何说明。

func mergedContent(t *testing.T, msgs ...Message) []Block {
	t.Helper()
	r := &Request{Model: "m", Messages: msgs}
	Sanitize(r)
	if len(r.Messages) == 0 {
		t.Fatal("消息被清空了")
	}
	return r.Messages[0].Content
}

func textOf(blocks []Block) string {
	var s string
	for _, b := range blocks {
		if b.Type == BlockText {
			s += b.Text
		}
	}
	return s
}

// TestDigitsAcrossMergeBoundaryDoNotFuse 是这条修复的来由：两段都以数字
// 收尾/起头时，无分隔拼接会造出一个两段都没说过的数字。
func TestDigitsAcrossMergeBoundaryDoNotFuse(t *testing.T) {
	got := textOf(mergedContent(t, userText("订单号 10086"), userText("3 件退货")))
	if got == "订单号 100863 件退货" {
		t.Fatalf("两段数字粘连成了一个新数字：%q", got)
	}
	if got != "订单号 10086\n\n3 件退货" {
		t.Errorf("拼接结果 = %q", got)
	}
}

// TestSeparatorNotAddedWhenClientAlreadyEndedWithWhitespace 钉住不重复加料：
// 客户端自己留了分隔时再插一个空行是改它的排版。
func TestSeparatorNotAddedWhenClientAlreadyEndedWithWhitespace(t *testing.T) {
	for _, tc := range []struct{ name, a, b, want string }{
		{"前段以换行收尾", "first\n", "second", "first\nsecond"},
		{"后段以换行起头", "first", "\nsecond", "first\nsecond"},
		{"前段以空格收尾", "first ", "second", "first second"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := textOf(mergedContent(t, userText(tc.a), userText(tc.b))); got != tc.want {
				t.Errorf("拼接结果 = %q，想要 %q", got, tc.want)
			}
		})
	}
}

// TestSeparatorOnlyBetweenTextBlocks 钉住只在文本对文本的边界插。
// 其余组合在出站编码里各自成块或成条，不会被拼进同一个字符串，
// 插一个空块反而会在 anthropic 那边多出一个空文本块。
func TestSeparatorOnlyBetweenTextBlocks(t *testing.T) {
	img := Block{Type: BlockImage, Media: &Media{Data: "AAAA"}}
	// 两侧各测一遍：判定是「两侧都是文本」的合取，只测一侧会让
	// 把 || 写成 && 这类错误从另一侧漏过去。
	for _, tc := range []struct {
		name       string
		first      Message
		second     Message
		wantBlocks int
	}{
		{
			name:       "前段以图片收尾",
			first:      Message{Role: RoleUser, Content: []Block{{Type: BlockText, Text: "look"}, img}},
			second:     userText("second"),
			wantBlocks: 3,
		},
		{
			name:       "后段以图片起头",
			first:      userText("look"),
			second:     Message{Role: RoleUser, Content: []Block{img, {Type: BlockText, Text: "second"}}},
			wantBlocks: 3,
		},
		{
			name:       "两侧都是图片",
			first:      Message{Role: RoleUser, Content: []Block{img}},
			second:     Message{Role: RoleUser, Content: []Block{img}},
			wantBlocks: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mergedContent(t, tc.first, tc.second)
			if len(got) != tc.wantBlocks {
				t.Fatalf("块数 = %d，想要 %d（无分隔块）：%#v", len(got), tc.wantBlocks, got)
			}
			for _, b := range got {
				if b.Type == BlockText && b.Text == "\n\n" {
					t.Errorf("非文本边界插了分隔块：%#v", got)
				}
			}
		})
	}
}

// TestEmptyTextDoesNotGetSeparator 钉住空文本块不触发分隔：空块拼出来
// 本就没有粘连风险，插一个空行等于凭空加了两个换行。
func TestEmptyTextDoesNotGetSeparator(t *testing.T) {
	got := textOf(mergedContent(t,
		Message{Role: RoleUser, Content: []Block{{Type: BlockText, Text: ""}}},
		userText("second")))
	if got != "second" {
		t.Errorf("拼接结果 = %q，想要 %q", got, "second")
	}
}

// TestThreeWayMergeSeparatesEveryBoundary 钉住每一道边界都插：只处理第一道
// 会让第三段仍然粘在第二段后面。
func TestThreeWayMergeSeparatesEveryBoundary(t *testing.T) {
	got := textOf(mergedContent(t, userText("a1"), userText("2b"), userText("3c")))
	if got != "a1\n\n2b\n\n3c" {
		t.Errorf("拼接结果 = %q", got)
	}
}
