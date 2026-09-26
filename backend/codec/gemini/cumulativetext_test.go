package gemini

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// textFrames 依次喂入若干「只含一个 text part」的帧，回收到达客户端的全部
// 正文增量（拼接）与整流注记。
func textFrames(t *testing.T, frames ...string) (string, []string) {
	t.Helper()
	dec := newStreamDecoder()
	var got strings.Builder
	for _, f := range frames {
		events, err := dec.Feed("", f)
		if err != nil {
			t.Fatalf("Feed(%q): %v", f, err)
		}
		for _, ev := range events {
			if ev.Type == ir.EvTextDelta {
				got.WriteString(ev.Text)
			}
		}
	}
	for _, ev := range dec.Finish() {
		if ev.Type == ir.EvTextDelta {
			got.WriteString(ev.Text)
		}
	}
	return got.String(), dec.Notes()
}

func part(text string) string {
	return `{"candidates":[{"content":{"parts":[{"text":` + jsonStr(text) + `}]}}]}`
}

func jsonStr(s string) string {
	// 用 Go 的转义把任意串塞进 JSON 字符串字面量（NUL 变 \u0000）。
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case 0:
			b.WriteString(`\u0000`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// TestIncrementalFramesPassThrough 正常增量上游：每帧只发新增部分，
// 逐帧原样放行，不得报累计/回退注记。
func TestIncrementalFramesPassThrough(t *testing.T) {
	got, notes := textFrames(t, part("Hello"), part(" "), part("world"))
	if got != "Hello world" {
		t.Errorf("增量帧应原样拼接，实得 %q", got)
	}
	if hasNoteSubstr(notes, "cumulative") || hasNoteSubstr(notes, "rewound") {
		t.Errorf("增量流不该报累计/回退注记：%v", notes)
	}
}

// TestCumulativeFramesForwardOnlySuffix 累计式上游：每帧重发全部文本，
// 只放行新增后缀，客户端看到的是不重复的完整正文。
func TestCumulativeFramesForwardOnlySuffix(t *testing.T) {
	got, notes := textFrames(t, part("Hello"), part("Hello wor"), part("Hello world"))
	if got != "Hello world" {
		t.Errorf("累计帧应压成后缀拼接为完整正文一次，实得 %q", got)
	}
	if !hasNoteSubstr(notes, "cumulative") {
		t.Errorf("累计式上游应报累计注记：%v", notes)
	}
}

// TestDuplicateFrameSwallowed 纯重复帧被吞掉，不产生第二次正文。
func TestDuplicateFrameSwallowed(t *testing.T) {
	got, notes := textFrames(t, part("Hello"), part("Hello"))
	if got != "Hello" {
		t.Errorf("重复帧应被吞掉，实得 %q", got)
	}
	if !hasNoteSubstr(notes, "rewound") && !hasNoteSubstr(notes, "duplicate") {
		t.Errorf("重复帧应报回退/重复注记：%v", notes)
	}
}

// TestRewoundFrameSwallowed 回退帧（比已下发短且是其前缀）被吞掉，
// 增量流不会倒带删字。
func TestRewoundFrameSwallowed(t *testing.T) {
	got, notes := textFrames(t, part("Hello world"), part("Hello"))
	if got != "Hello world" {
		t.Errorf("回退帧应被吞掉，实得 %q", got)
	}
	if !hasNoteSubstr(notes, "rewound") {
		t.Errorf("回退帧应报回退注记：%v", notes)
	}
}

// TestNulStrippedFromText 正文里的 NUL 字节被剥除并报注记，
// 其余文本保留。
func TestNulStrippedFromText(t *testing.T) {
	got, notes := textFrames(t, part("a\x00b"), part("\x00c"))
	if got != "abc" {
		t.Errorf("NUL 应被剥除，实得 %q", got)
	}
	if !hasNoteSubstr(notes, "NUL") {
		t.Errorf("含 NUL 应报剥除注记：%v", notes)
	}
}

// TestNulOnlyPartSkipped 只含 NUL 的 part 剥完为空，整 part 跳过不开块。
func TestNulOnlyPartSkipped(t *testing.T) {
	got, notes := textFrames(t, part("\x00\x00"))
	if got != "" {
		t.Errorf("纯 NUL part 应被跳过，实得 %q", got)
	}
	if !hasNoteSubstr(notes, "NUL") {
		t.Errorf("纯 NUL part 仍应报剥除注记：%v", notes)
	}
}

// TestNonStreamingNulStripped 非流式路径同样剥 NUL 并报注记。
func TestNonStreamingNulStripped(t *testing.T) {
	body := []byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"a\u0000b"}]}}]}`)
	resp, notes, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "ab" {
		t.Errorf("非流式应剥 NUL，实得 %+v", resp.Content)
	}
	if !hasNoteSubstr(notes, "NUL") {
		t.Errorf("非流式含 NUL 应报注记：%v", notes)
	}
}

func hasNoteSubstr(notes []string, want string) bool {
	for _, n := range notes {
		if strings.Contains(n, want) {
			return true
		}
	}
	return false
}
