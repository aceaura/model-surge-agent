package codec_test

import (
	"testing"
)

// D3：纯垃圾帧（外层 JSON 都解不开、取不出任何正文）跳过续流——它前后的正常
// 帧照常解出，整流不终止，收尾恰报一条跳帧注记。SSE 以事件边界自同步，坏一帧
// 不污染后续帧；终止整流会让坏帧之后的全部正常正文一起陪葬，那才是真的丢内容。
//
// 与「残缺多文档行 fail-fast」（TestMultiDocLineMatrix）分账：那种行首已有完整
// 文档、带着正文，丢掉它而不报错正是本仓最忌讳的静默缺失，所以仍终止。
func TestBadFrameIsSkippedAndStreamContinues(t *testing.T) {
	for _, up := range outboundNames() {
		pair, ok := multiDocLines[up]
		if !ok {
			continue
		}
		t.Run(up, func(t *testing.T) {
			// 中间那帧第一个 JSON 文档就解不开（截断的 `{"type":`），属纯垃圾。
			raw := "data: " + pair[0] + "\n\n" +
				"data: {\"type\":\n\n" +
				"data: " + pair[1] + "\n\n"
			resp, notes := decodeStreamWithNotes(t, up, raw)
			if got := responseText(resp); got != "ab" {
				t.Errorf("坏帧前后的正文都该解出，文本 = %q，应为 ab", got)
			}
			if !hasNoteContaining(notes, "malformed stream frame") {
				t.Errorf("跳帧必须留注记，实得 %v", notes)
			}
		})
	}
}

// 多个垃圾帧累计成一条注记里的计数，而不是逐帧各报一条（去重挡不住不同数字）。
func TestBadFrameSkipCountAccumulates(t *testing.T) {
	pair := multiDocLines["anthropic"]
	raw := "data: {\"type\":\n\n" +
		"data: not json at all\n\n" +
		"data: " + pair[0] + "\n\n" +
		"data: {\n\n"
	resp, notes := decodeStreamWithNotes(t, "anthropic", raw)
	if got := responseText(resp); got != "a" {
		t.Errorf("正常帧仍该解出，文本 = %q，应为 a", got)
	}
	var hits int
	for _, n := range notes {
		if hasNoteContaining([]string{n}, "malformed stream frame") {
			hits++
		}
	}
	if hits != 1 {
		t.Errorf("跳帧注记应恰一条（含累计计数），实得 %d 条：%v", hits, notes)
	}
}
