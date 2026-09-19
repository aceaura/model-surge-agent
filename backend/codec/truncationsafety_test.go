package codec

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 上游回中文错误页时，StatusMessage 的产物必须是合法 UTF-8。
//
// 这个产物流入 ir.Error.Message → rec.ErrorMessage → request_log.error_message
// （TEXT 列）。切在多字节字符中间会让 PG 拒收整行，症状是那一次故障的流水
// 恰好缺失，而客户端那侧收到的错误信封看起来完全正常。
func TestStatusMessageKeepsValidUTF8(t *testing.T) {
	// 512 是 StatusMessage 的上限。用三字节字符铺满，保证切点落在字符中间：
	// 512 不是 3 的倍数。
	body := strings.Repeat("额", 400)
	got := StatusMessage(502, []byte(body))
	if !utf8.ValidString(got) {
		t.Fatalf("StatusMessage 产出非法 UTF-8: %q", got)
	}
	if !strings.Contains(got, "额") {
		t.Errorf("截断把内容整段丢了: %q", got)
	}
	// 上限本身也要钉住：这条消息进 TEXT 列，而「不限长」与「切坏了」
	// 是同一个来源（上游可能回一整张 HTML 页）的两种坏结果。
	// 512 是上限，前缀 "upstream returned 502: " 另计。
	if n := len(got) - len("upstream returned 502: "); n > 512 {
		t.Errorf("上游原文占了 %d 字节，超过 512 的上限", n)
	}
}

// 上限内的错误体原样带上，不因为本轮改动而被动过。
func TestStatusMessageShortBodyUnchanged(t *testing.T) {
	got := StatusMessage(429, []byte("配额不足"))
	if !strings.Contains(got, "配额不足") {
		t.Errorf("StatusMessage = %q，丢了上游原文", got)
	}
}

// 上游收尾原因的说明同样走截断点。
//
// 这条说明进 lossy 那个 JSONB 列，而它带的是上游原文——那是全仓唯一一处
// 「本服务构造的诊断里嵌了上游字节」的地方。
func TestFinishDetailNoteKeepsValidUTF8(t *testing.T) {
	got := FinishDetailNote(strings.Repeat("策", 200))
	if !utf8.ValidString(got) {
		t.Fatalf("FinishDetailNote 产出非法 UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("超长时没标出被截断: %q", got)
	}
}

// 不超上限的原文一个字节都不动：这条说明的全部价值就在原文里。
func TestFinishDetailNoteKeepsShortDetailVerbatim(t *testing.T) {
	const detail = "触发了安全策略 S-17"
	got := FinishDetailNote(detail)
	if !strings.HasSuffix(got, detail) {
		t.Errorf("FinishDetailNote = %q，没原样带上 %q", got, detail)
	}
}
