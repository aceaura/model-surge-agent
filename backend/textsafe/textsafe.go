// Package textsafe 处理来自上游的文本在跨越字节边界时的完整性。
//
// 独立成叶子包而不放进任何现有包：四个调用点分布在 codec、relayclient、
// store 三个互不依赖的包里，塞进其中任何一个都会造出一条本不该有的依赖边。
package textsafe

import (
	"strings"
	"unicode/utf8"
)

// Truncate 把 s 截到不超过 maxBytes 字节，并保证结果是合法 UTF-8。
//
// 按字节切会切在多字节字符中间。后果不在客户端那一侧——Go 的 json.Marshal
// 把坏字节替成 U+FFFD 且不报错——而在 PG：非法序列让整行流水被拒
// （SQLSTATE 22021），于是那一次故障的记录恰好缺失，缺的正是最需要的那条。
//
// 往回退而不是往前补：上限的含义是「最多这么多字节」，为凑齐一个完整字符
// 而超出会把约束反过来，而 outbox.last_error 那一处上限对着的是真实存储。
// 最多退 3 字节，UTF-8 单字符最长 4 字节。
//
// 用 ValidString 判而不是看末尾 rune 是否 RuneError：合法的 U+FFFD 本身就
// 解码成 RuneError，那样会把上游真的发过来的替换字符误判成截断残骸砍掉。
func Truncate(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := s[:maxBytes]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// Clean 剔除 s 里的非法 UTF-8 字节，保留其余内容。
//
// 与 Truncate 管的是两件事：Truncate 管「我们自己切坏的」，Clean 管「上游
// 本来就发的坏字节」——解压失败后退回原始字节那条路上的内容从不经过任何
// 截断点。只做一件挡不住另一件。
//
// 替成空串而不是 U+FFFD：这些文本要么进日志要么进 PG 的 TEXT 列，留一串
// 替换字符只是把「坏了」这个事实搬到人眼前占位。
func Clean(s string) string {
	return strings.ToValidUTF8(s, "")
}
