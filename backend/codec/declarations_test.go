package codec

import (
	"reflect"
	"testing"
)

func TestParseBetas(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  []string
	}{
		{"nil", nil, nil},
		{"empty line", []string{""}, nil},
		{"only separators", []string{" , , "}, nil},
		{"single", []string{"a-1"}, []string{"a-1"}},
		{"comma list", []string{"a-1,b-2"}, []string{"a-1", "b-2"}},
		{"trims", []string{" a-1 ,\tb-2 "}, []string{"a-1", "b-2"}},
		// 多行：客户端可以分多个同名头发，只取第一行会静默丢掉其余声明。
		{"multi line", []string{"a-1", "b-2"}, []string{"a-1", "b-2"}},
		{"multi line with commas", []string{"a-1,b-2", "c-3"}, []string{"a-1", "b-2", "c-3"}},
		// 去重保序：顺序是客户端的表达，重排没有收益却让对账变难。
		{"dedupe keeps first order", []string{"z-1,a-2,z-1,m-3"}, []string{"z-1", "a-2", "m-3"}},
		{"dedupe across lines", []string{"a-1", "a-1"}, []string{"a-1"}},
		// 大小写不折叠：令牌是上游定义的字面量，我们无权判定两种写法等价。
		{"case is preserved", []string{"A-1,a-1"}, []string{"A-1", "a-1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ParseBetas(c.lines); !reflect.DeepEqual(got, c.want) {
				t.Errorf("ParseBetas(%q) = %q, want %q", c.lines, got, c.want)
			}
		})
	}
}

func TestDeclarationsEmpty(t *testing.T) {
	cases := []struct {
		name string
		d    Declarations
		want bool
	}{
		{"zero", Declarations{}, true},
		{"empty slice", Declarations{Betas: []string{}}, true},
		{"version only", Declarations{APIVersion: "2024-10-22"}, false},
		{"betas only", Declarations{Betas: []string{"a-1"}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.d.Empty(); got != c.want {
				t.Errorf("Empty() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestDescribeDeclarationLossOnNilOutbound 钉住不 panic：
// 有损诊断是排查用的辅助路径，它自己崩掉会掩盖真正要查的问题。
func TestDescribeDeclarationLossOnNilOutbound(t *testing.T) {
	notes := DescribeDeclarationLoss(Declarations{APIVersion: "2024-10-22"}, nil)
	if len(notes) != 1 {
		t.Fatalf("notes = %v, want exactly 1", notes)
	}
}
