package pipeline

import (
	"reflect"
	"testing"
)

// 请求侧的整体覆盖不得连带擦掉响应侧说明。覆盖发生在换目标重试时
// （pipeline 编码阶段直接给 Lossy 赋值），响应侧说明描述的是另一件事，
// 被一起抹掉就再也查不出客户端看到的内容为何缺失。
func TestResponseNotesSurviveRequestSideOverwrite(t *testing.T) {
	rec := Record{}
	rec.addResponseLossy("dropped thinking signature (foreign protocol family)")

	// 模拟第一个目标的请求侧说明，随后换目标整体覆盖。
	rec.Lossy = []string{"dropped top_k (target A cannot express it)"}
	rec.Lossy = []string{"dropped tools (target B cannot express it)"}

	got := rec.mergedLossy()
	want := []string{
		"dropped thinking signature (foreign protocol family)",
		"dropped tools (target B cannot express it)",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("merged lossy = %#v, want %#v", got, want)
	}
}

// 两侧都为空时必须返回 nil：对外的 lossy 字段靠空值判定是否出现，
// 返回空切片会让每条无损流水都多一个空数组。
func TestMergedLossyEmptyStaysNil(t *testing.T) {
	rec := Record{}
	if got := rec.mergedLossy(); got != nil {
		t.Errorf("merged lossy = %#v, want nil", got)
	}
}

// 响应侧累加而非覆盖：一个流里同类丢弃会发生多次，但对外只报一条。
func TestResponseNotesAccumulateAndDeduplicate(t *testing.T) {
	rec := Record{}
	rec.addResponseLossy("dropped thinking signature (foreign protocol family)")
	rec.addResponseLossy("dropped thinking signature (foreign protocol family)")
	rec.addResponseLossy("dropped redacted_thinking block")

	got := rec.mergedLossy()
	want := []string{
		"dropped redacted_thinking block",
		"dropped thinking signature (foreign protocol family)",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("merged lossy = %#v, want %#v", got, want)
	}
}
