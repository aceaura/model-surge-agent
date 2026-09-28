package ir

import "testing"

// CountStrayPortableCitations：只数「非 assistant 消息上的可移植引用」。
// 可移植=有 URL；assistant 消息上的不计；非可移植（无 URL）的不计（那类归
// CountNonPortableCitations）。
func TestCountStrayPortableCitations(t *testing.T) {
	portable := func(u string) Citation { return Citation{URL: u} }
	nonPortable := Citation{CitedText: "x", Start: 0, End: 1}

	r := &Request{Messages: []Message{
		// user：2 可移植 + 1 非可移植 → 计 2
		{Role: RoleUser, Content: []Block{{Type: BlockText,
			Citations: []Citation{portable("a"), portable("b"), nonPortable}}}},
		// assistant：1 可移植 → 不计
		{Role: RoleAssistant, Content: []Block{{Type: BlockText,
			Citations: []Citation{portable("c")}}}},
		// user：1 非可移植 → 不计
		{Role: RoleUser, Content: []Block{{Type: BlockText,
			Citations: []Citation{nonPortable}}}},
		// user：跨块 1 可移植 → 计 1
		{Role: RoleUser, Content: []Block{
			{Type: BlockText},
			{Type: BlockText, Citations: []Citation{portable("d")}},
		}},
	}}
	if got := CountStrayPortableCitations(r); got != 3 {
		t.Errorf("want 3 stray portable citations, got %d", got)
	}
	// 与 CountNonPortableCitations 不重叠：后者数无 URL 的（共 2 条），
	// 前者数非 assistant 上有 URL 的（共 3 条），互斥。
	if got := CountNonPortableCitations(r); got != 2 {
		t.Errorf("want 2 non-portable citations, got %d", got)
	}
}

// 空请求 / 全 assistant / 全非可移植都为 0。
func TestCountStrayPortableCitationsZero(t *testing.T) {
	cases := map[string]*Request{
		"nil messages": {},
		"all assistant": {Messages: []Message{
			{Role: RoleAssistant, Content: []Block{{Type: BlockText,
				Citations: []Citation{{URL: "a"}}}}},
		}},
		"all non-portable": {Messages: []Message{
			{Role: RoleUser, Content: []Block{{Type: BlockText,
				Citations: []Citation{{CitedText: "x"}}}}},
		}},
		"no citations": {Messages: []Message{
			{Role: RoleUser, Content: []Block{{Type: BlockText, Text: "x"}}},
		}},
	}
	for name, r := range cases {
		if got := CountStrayPortableCitations(r); got != 0 {
			t.Errorf("%s: want 0, got %d", name, got)
		}
	}
}
