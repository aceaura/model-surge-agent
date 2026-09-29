package ir

import "testing"

// CountSystemCitations：只数系统提示（req.System）块上的引用，与三个只遍历
// Messages 的计数器分账——System 是独立的 []Block，不挂在任何消息角色上。
// 系统提示引用不分可移植与否一律计入（三外族都把 system 收敛成纯文本、整体无槽）。
func TestCountSystemCitations(t *testing.T) {
	portable := func(u string) Citation { return Citation{URL: u} }
	nonPortable := Citation{CitedText: "x", Start: 0, End: 1}

	r := &Request{
		// 系统提示：跨三块共 2 可移植 + 1 非可移植 → 全计 3。
		System: []Block{
			{Type: BlockText, Citations: []Citation{portable("a"), nonPortable}},
			{Type: BlockText},
			{Type: BlockText, Citations: []Citation{portable("b")}},
		},
		// Messages 上的引用不计入 System 计数（分账，避免与 stray/document 重复）。
		Messages: []Message{
			{Role: RoleUser, Content: []Block{{Type: BlockText,
				Citations: []Citation{portable("m1"), portable("m2")}}}},
		},
	}
	if got := CountSystemCitations(r); got != 3 {
		t.Errorf("want 3 system citations, got %d", got)
	}
	// 与 Messages 级计数器互不重叠：CountCitations 只数 Messages（2 条），
	// 看不见 System；CountSystemCitations 只数 System（3 条），看不见 Messages。
	if got := CountCitations(r); got != 2 {
		t.Errorf("want 2 message citations (System excluded), got %d", got)
	}
}

// 空 System / System 无引用 / nil 请求 / 只有 Messages 都为 0。
func TestCountSystemCitationsZero(t *testing.T) {
	cases := map[string]*Request{
		"nil":            {},
		"empty system":   {System: []Block{}},
		"system no cite": {System: []Block{{Type: BlockText, Text: "x"}}},
		"only messages": {Messages: []Message{
			{Role: RoleUser, Content: []Block{{Type: BlockText,
				Citations: []Citation{{URL: "a"}}}}},
		}},
	}
	for name, r := range cases {
		if got := CountSystemCitations(r); got != 0 {
			t.Errorf("%s: want 0, got %d", name, got)
		}
	}
}
