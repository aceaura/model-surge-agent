package ir

import "testing"

// ServerParams 是指针槽位：Clone 必须换头并复制切片容器，改副本不得穿透
// 到原件（换目标重试的两份编码各自独立）。
func TestCloneDeepCopiesServerParams(t *testing.T) {
	src := &Request{
		Model: "m", MaxTokens: 8,
		Tools: []Tool{{
			Name: "web_search", ServerType: "web_search_20250305",
			ServerParams: &ServerParams{
				MaxUses:        3,
				AllowedDomains: []string{"a.com"},
				BlockedDomains: []string{"c.com"},
				UserLocation:   []byte(`{"type":"approximate"}`),
			},
		}},
	}
	dst := src.Clone()
	if dst.Tools[0].ServerParams == src.Tools[0].ServerParams {
		t.Fatal("Clone 共享了 ServerParams 指针")
	}
	p := dst.Tools[0].ServerParams
	p.MaxUses = 99
	p.AllowedDomains[0] = "mutated"
	p.AllowedDomains = append(p.AllowedDomains, "extra")
	p.BlockedDomains[0] = "mutated"
	q := src.Tools[0].ServerParams
	if q.MaxUses != 3 || q.AllowedDomains[0] != "a.com" || len(q.AllowedDomains) != 1 ||
		q.BlockedDomains[0] != "c.com" {
		t.Errorf("Clone 穿透改到了原件：%+v", q)
	}
}

// 没给参数的工具 Clone 后保持 nil——不得凭空造出空结构，
// nil 与「全零值结构」在编码侧语义不同（缺省保持缺省）。
func TestCloneKeepsServerParamsNil(t *testing.T) {
	src := &Request{Model: "m", MaxTokens: 8,
		Tools: []Tool{{Name: "web_search", ServerType: "web_search_20250305"}}}
	dst := src.Clone()
	if dst.Tools[0].ServerParams != nil {
		t.Errorf("nil 被克隆成空结构：%+v", dst.Tools[0].ServerParams)
	}
}
