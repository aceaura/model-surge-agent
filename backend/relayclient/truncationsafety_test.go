package relayclient

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 调度层回非 JSON 时的兜底消息必须是合法 UTF-8。
//
// 这条消息会经 outbox.last_error 落到 PG 的 TEXT 列，而那一列存在的意义就是
// 记下这次上报为什么失败——因为切坏了字符而写不进去，丢的正是最需要的那条。
func TestSnippetKeepsValidUTF8(t *testing.T) {
	// 256 是 snippet 的上限，不是 3 的倍数，所以三字节字符铺满必然切在字符中间。
	body := strings.Repeat("网", 200)
	err := decodeError(502, []byte(body))
	if !utf8.ValidString(err.Message) {
		t.Fatalf("兜底消息非法 UTF-8: %q", err.Message)
	}
	if !strings.Contains(err.Message, "网") {
		t.Errorf("截断把内容整段丢了: %q", err.Message)
	}
}

// 上限内的响应体原样带上。
func TestSnippetShortBodyUnchanged(t *testing.T) {
	err := decodeError(503, []byte("网关暂时不可用"))
	if !strings.Contains(err.Message, "网关暂时不可用") {
		t.Errorf("兜底消息 = %q，丢了原文", err.Message)
	}
}

// 上游本来就发的坏字节也不能让消息变成非法 UTF-8 之后一路传下去。
//
// 这一条与「我们切坏的」是两件事：这里的输入没超上限，根本没走截断。
// 挡住它的是写库前的净化，而不是 snippet——所以这条测试只钉「不会崩、
// 内容还在」，真正的落库判据在 store 的真 PG 测试里。
func TestSnippetToleratesInvalidUpstreamBytes(t *testing.T) {
	err := decodeError(500, []byte("坏\xe9字节"))
	if !strings.Contains(err.Message, "字节") {
		t.Errorf("兜底消息 = %q，丢了坏字节之后的内容", err.Message)
	}
}
