package textsafe

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 截断多字节文本必须产出合法 UTF-8。
//
// 坏处不在客户端那一侧——json.Marshal 把坏字节替成 U+FFFD 且不报错——而在
// PG：非法序列让整行流水被拒（实测 SQLSTATE 22021），于是那一次故障的记录
// 恰好缺失。
func TestTruncateLandsOnRuneBoundary(t *testing.T) {
	s := "上游错误：配额不足，请稍后再试"
	// 逐个可能的切点都试：总有几个落在多字节字符中间。
	for n := 0; n <= len(s); n++ {
		got := Truncate(s, n)
		if !utf8.ValidString(got) {
			t.Fatalf("Truncate(%d) = %q，不是合法 UTF-8", n, got)
		}
		if len(got) > n {
			t.Fatalf("Truncate(%d) 回了 %d 字节，超过上限", n, len(got))
		}
	}
}

// 上限内的输入原样返回，一个字节都不动。
func TestTruncateLeavesShortInputAlone(t *testing.T) {
	for _, s := range []string{"", "abc", "中文", strings.Repeat("x", 512)} {
		if got := Truncate(s, 512); got != s {
			t.Errorf("Truncate(%q) = %q，本该原样返回", s, got)
		}
	}
}

// 退，而不是补。
//
// 为凑齐一个完整字符而超出上限会把约束反过来：outbox.last_error 那一处
// 上限对着的是一列真实存储。
func TestTruncateShrinksNeverGrows(t *testing.T) {
	// "中" 是三字节，切到 2 必须退到 0 而不是进到 3。
	if got := Truncate("中文", 2); got != "" {
		t.Errorf("Truncate(\"中文\", 2) = %q, want 空串", got)
	}
	if got := Truncate("中文", 4); got != "中" {
		t.Errorf("Truncate(\"中文\", 4) = %q, want \"中\"", got)
	}
}

// 输入里合法的 U+FFFD 不能被当成截断残骸砍掉。
//
// 这是不用「末尾 rune 是否 RuneError」判定的理由：合法的替换字符本身就解码
// 成 RuneError，那样会砍掉上游真的发过来的内容。
func TestTruncateKeepsGenuineReplacementChar(t *testing.T) {
	s := "a\uFFFDb"
	if got := Truncate(s, len(s)); got != s {
		t.Errorf("Truncate 砍掉了合法的 U+FFFD: %q", got)
	}
	// 刚好切在 U+FFFD 之后：它必须留着。
	cut := len("a\uFFFD")
	if got := Truncate(s, cut); got != "a\uFFFD" {
		t.Errorf("Truncate(%d) = %q, want %q", cut, got, "a\uFFFD")
	}
}

// Clean 只剔坏字节，不丢整段。
//
// 整段丢弃等于把那次故障的全部线索换成空白，而线索恰恰在剩下的部分里。
func TestCleanKeepsEverythingElse(t *testing.T) {
	bad := "上游错误\xe9：配额不足"
	got := Clean(bad)
	if !utf8.ValidString(got) {
		t.Fatalf("Clean 之后仍非法: %q", got)
	}
	if !strings.Contains(got, "配额不足") {
		t.Errorf("Clean = %q，丢掉了坏字节以外的内容", got)
	}
	if !strings.Contains(got, "上游错误") {
		t.Errorf("Clean = %q，丢掉了坏字节之前的内容", got)
	}
}

// 替成空串而不是 U+FFFD：留一串替换字符只是把「坏了」搬到人眼前占位。
func TestCleanDropsRatherThanSubstitutes(t *testing.T) {
	if got := Clean("a\xffb"); got != "ab" {
		t.Errorf("Clean = %q, want \"ab\"", got)
	}
}

// 合法输入不动。Clean 会被放在写库路径上，对绝大多数请求都必须是恒等的。
func TestCleanLeavesValidInputAlone(t *testing.T) {
	for _, s := range []string{"", "abc", "上游错误：配额不足", "a\uFFFDb"} {
		if got := Clean(s); got != s {
			t.Errorf("Clean(%q) = %q，本该原样返回", s, got)
		}
	}
}
