package ir

import "testing"

// 多轮历史里同 id 的两次 tool_use 是本轮要治理的畸形：索引只留最后一处，
// 前一轮的调用从此没有宣告方，它的结果被当成重复结果降级成文本，
// 后一轮的结果绑到前一轮的调用上，参数与结果就对错了。
func TestDuplicateToolUseIDAcrossTurnsIsReported(t *testing.T) {
	r := &Request{
		Model: "m",
		Messages: []Message{
			userText("turn 1"),
			assistantCalls("same"),
			userResults("same"),
			userText("turn 2"),
			assistantCalls("same"),
			userResults("same"),
		},
	}
	notes := Sanitize(r)
	if !hasNote(notes, `duplicate tool_use id "same"`) {
		t.Fatalf("notes = %v，同 id 的跨轮调用必须点出根因，否则症状是「结果不见了」", notes)
	}
}

// 说明必须与「重复结果降级」分开：两者症状相同（少了一份结果），
// 根因一个在 id 合成、一个在客户端重放，混成一句话就分不出该改哪边。
func TestDuplicateUseNoteIsDistinctFromDuplicateResultNote(t *testing.T) {
	r := &Request{
		Model: "m",
		Messages: []Message{
			assistantCalls("same"),
			userResults("same"),
			assistantCalls("same"),
			userResults("same"),
		},
	}
	notes := Sanitize(r)
	if !hasNote(notes, "duplicate tool_use id") {
		t.Errorf("notes = %v，缺少 tool_use 撞 id 的说明", notes)
	}
	if !hasNote(notes, "duplicate tool_result") {
		t.Errorf("notes = %v，缺少重复结果降级的说明", notes)
	}
}

// 不同 id 的两轮是正常历史：不得报任何说明，也不得改动请求。
// 这正是合成 id 带上响应 scope 之后应有的形态。
func TestDistinctToolUseIDsAcrossTurnsStayClean(t *testing.T) {
	r := &Request{
		Model: "m",
		Messages: []Message{
			userText("turn 1"),
			assistantCalls("msa_synth_aaaaaa_grep_1"),
			userResults("msa_synth_aaaaaa_grep_1"),
			userText("turn 2"),
			assistantCalls("msa_synth_bbbbbb_grep_1"),
			userResults("msa_synth_bbbbbb_grep_1"),
		},
	}
	notes := Sanitize(r)
	if len(notes) != 0 {
		t.Fatalf("跨轮 id 不同却产出诊断：%v", notes)
	}
	uses, results := toolIDs(r)
	if len(uses) != 2 || len(results) != 2 {
		t.Fatalf("uses = %v, results = %v，两轮的调用与结果都该留下", uses, results)
	}
	for i := range uses {
		if uses[i] != results[i] {
			t.Errorf("第 %d 对配错了：调用 %q 结果 %q", i, uses[i], results[i])
		}
	}
}

// 撞 id 的实际损坏形态：前一轮的结果被降级成文本。
// 断言这个后果而不只断言说明，否则说明改了措辞就测不到损坏本身。
func TestCollidingIDsCostOneToolResult(t *testing.T) {
	r := &Request{
		Model: "m",
		Messages: []Message{
			assistantCalls("same"),
			userResults("same"),
			assistantCalls("same"),
			userResults("same"),
		},
	}
	Sanitize(r)
	_, results := toolIDs(r)
	if len(results) != 1 {
		t.Fatalf("results = %v，撞 id 后只该剩一份结果（这就是损坏本身）", results)
	}
}
