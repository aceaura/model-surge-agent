package codec

import (
	"strconv"
	"strings"
	"testing"
)

// 上限为零即跳过测量：四协议当前都是零值，
// 没有实测证据的上限不该拿来判一个请求超不超。
func TestPayloadBudgetNoteSkipsWithoutLimit(t *testing.T) {
	body := []byte(strings.Repeat("x", 1<<20))
	if note := PayloadBudgetNote(body, "probe", Capabilities{}); note != "" {
		t.Errorf("上限为零仍报了说明：%q", note)
	}
}

// 恰好等于上限不算超：上限是「可接受的最大值」而非「必须小于」。
func TestPayloadBudgetNoteAllowsExactLimit(t *testing.T) {
	body := []byte(strings.Repeat("x", 100))
	if note := PayloadBudgetNote(body, "probe", Capabilities{MaxPayloadBytes: 100}); note != "" {
		t.Errorf("恰好等于上限报了说明：%q", note)
	}
}

// 超限要报说明，且说明里必须同时有实测值与上限值——
// 只说「超了」排查者还得自己去量。
func TestPayloadBudgetNoteReportsBothNumbers(t *testing.T) {
	body := []byte(strings.Repeat("x", 150))
	note := PayloadBudgetNote(body, "probe", Capabilities{MaxPayloadBytes: 100})
	if note == "" {
		t.Fatal("超限未报说明")
	}
	for _, want := range []string{strconv.Itoa(150), strconv.Itoa(100), "probe"} {
		if !strings.Contains(note, want) {
			t.Errorf("说明里缺 %q：%q", want, note)
		}
	}
	// 实测中这类超限回的是 reason 为空的 400，措辞要点出这一点，
	// 否则排查者不会把 Improperly formed request 与体积联系起来。
	if !strings.Contains(note, "misleading 400") {
		t.Errorf("说明未点明误导性 400：%q", note)
	}
}
