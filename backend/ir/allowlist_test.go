package ir

import (
	"reflect"
	"testing"
)

// 白名单只在「模型自由挑工具」的两档上有事可做：指名调用上游本来就只调
// 那一个，禁止调用一个都不调，收窄对它们是空转，算进来会恒报「限制失效」。
func TestAllowlistAppliesOnlyToOpenModes(t *testing.T) {
	for _, mode := range []ToolChoiceMode{ToolChoiceAuto, ToolChoiceAny} {
		tc := &ToolChoice{Mode: mode, AllowedTools: []string{"alpha"}}
		if !tc.AllowlistApplies() {
			t.Errorf("%s：应适用白名单", mode)
		}
	}
	for _, mode := range []ToolChoiceMode{ToolChoiceNone, ToolChoiceTool} {
		tc := &ToolChoice{Mode: mode, Name: "alpha", AllowedTools: []string{"alpha"}}
		if tc.AllowlistApplies() {
			t.Errorf("%s：不应适用白名单", mode)
		}
	}
	var nilTC *ToolChoice
	if nilTC.AllowlistApplies() {
		t.Error("nil 接收者不应适用")
	}
	if (&ToolChoice{Mode: ToolChoiceAuto}).AllowlistApplies() {
		t.Error("空白名单不应适用")
	}
}

// 收窄只留白名单里的函数工具；服务端工具恒保留（白名单管的是客户端声明
// 的函数）；白名单里未声明的名字（ghost）不碍事。
func TestAllowlistNarrowDropsUnlistedTools(t *testing.T) {
	r := &Request{
		Tools: []Tool{
			{Name: "alpha"}, {Name: "beta"}, {Name: "gamma"},
			{Name: "web_search", ServerType: "web_search"},
		},
		ToolChoice: &ToolChoice{Mode: ToolChoiceAny, AllowedTools: []string{"alpha", "ghost"}},
	}
	kept, ok := r.AllowlistNarrow()
	if !ok {
		t.Fatal("交集非空，应落得下去")
	}
	var names []string
	for _, tl := range kept {
		names = append(names, tl.Name)
	}
	if !reflect.DeepEqual(names, []string{"alpha", "web_search"}) {
		t.Errorf("kept = %v", names)
	}
}

// 交集全空时无从收窄：原样返回整个列表。收窄到零个工具会连锁触发
// 「零工具丢 tool_choice」，那是比白名单失效大得多的破坏。
func TestAllowlistNarrowRefusesEmptyIntersection(t *testing.T) {
	r := &Request{
		Tools:      []Tool{{Name: "alpha"}, {Name: "beta"}},
		ToolChoice: &ToolChoice{Mode: ToolChoiceAuto, AllowedTools: []string{"ghost"}},
	}
	kept, ok := r.AllowlistNarrow()
	if ok {
		t.Fatal("交集为空，不应收窄")
	}
	if len(kept) != 2 {
		t.Errorf("应原样返回整个列表，kept = %v", kept)
	}
}

// 只有服务端工具时交集同样为空（服务端工具不计入交集）：不收窄。
func TestAllowlistNarrowServerToolsDoNotMatch(t *testing.T) {
	r := &Request{
		Tools:      []Tool{{Name: "web_search", ServerType: "web_search"}},
		ToolChoice: &ToolChoice{Mode: ToolChoiceAuto, AllowedTools: []string{"web_search"}},
	}
	if _, ok := r.AllowlistNarrow(); ok {
		t.Error("服务端工具不计入交集，应收窄失败")
	}
}

func TestAllowlistNarrowInertWithoutAllowlist(t *testing.T) {
	r := &Request{Tools: []Tool{{Name: "alpha"}}, ToolChoice: &ToolChoice{Mode: ToolChoiceAuto}}
	if kept, ok := r.AllowlistNarrow(); ok || kept != nil {
		t.Errorf("= %v, %v，白名单缺席时应不动", kept, ok)
	}
	var nilReq *Request
	if _, ok := nilReq.AllowlistNarrow(); ok {
		t.Error("nil 接收者不应适用")
	}
}

// Clone 必须换头 AllowedTools：换目标重试时两条路径各自整形，共享底层
// 切片会串味。
func TestCloneCopiesAllowedTools(t *testing.T) {
	src := &Request{ToolChoice: &ToolChoice{Mode: ToolChoiceAuto, AllowedTools: []string{"a"}}}
	got := src.Clone()
	got.ToolChoice.AllowedTools[0] = "mutated"
	if src.ToolChoice.AllowedTools[0] != "a" {
		t.Errorf("Clone 共享了底层切片：%v", src.ToolChoice.AllowedTools)
	}
}
