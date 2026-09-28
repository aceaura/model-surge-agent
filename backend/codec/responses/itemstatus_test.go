package responses

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
)

// 轮次67：responses 上游在每个 output 条目上给出条目级 status（官方
// ResponseOutputMessage.status / FunctionToolCall.status，均为**必填**，枚举
// in_progress|completed|incomplete），标记该条目自身是生成中、已完成还是被截断
// （如 max_output_tokens 在条目中途截停）。此前 wireRespItem.Status 建模了却从不被
// 任何解码路径读取，且本族编码器给每个条目一律合成 "completed"
// （openItem.wire / EncodeResponse 多处），于是上游标为 incomplete 的条目到客户端被
// 静默**改写**成 completed——既是丢弃也是改写，无注记。与响应级 status（stopReasonFor
// 已保全整体截断信号，见 statusfidelity_test.go）互补而非重复：这里丢的是「究竟哪个
// 条目没写完」的条目粒度信息。
//
// 与 phase 同款「上游给了、IR 无槽位」处置：只探测计数、经 ResponseItemStatusDropNote
// 报出。判据：只在 status **未完成**（非空且非 completed）时计数——completed 是终态
// 响应里每个条目的常态、且被如实改写回 completed（无丢失），无条件计入会对每条正常
// 条目误报（违反规则 a：假阳性与漏报同样是缺口）。
//
// 指纹子串 "per-item status slot" 为 ResponseItemStatusDropNote 独有。

const r67fingerprint = "per-item status slot"

// 非流式：message 条目 status=incomplete → 注记报出，计数 1。
func TestItemStatusNonStreamingIncompleteNoted(t *testing.T) {
	body := `{"id":"r1","object":"response","status":"incomplete",
	  "incomplete_details":{"reason":"max_output_tokens"},"output":[
      {"type":"message","role":"assistant","status":"incomplete",
       "content":[{"type":"output_text","text":"truncated mid-sent"}]}]}`
	_, notes, err := DecodeResponseLossy([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if !anyNoteHas(notes, r67fingerprint) {
		t.Fatalf("非流式 incomplete 条目状态没被报出：%v", notes)
	}
	if !anyNoteHas(notes, codec.ResponseItemStatusDropNote(1)) {
		t.Errorf("want a count of exactly 1, got %v", notes)
	}
}

// 非流式：function_call 条目 status=incomplete 也计（status 官方对 function_call
// 同样必填，探针在 switch 之前对所有条目类型生效）。
func TestItemStatusNonStreamingFunctionCallNoted(t *testing.T) {
	body := `{"id":"r1","object":"response","status":"incomplete","output":[
      {"type":"function_call","call_id":"c1","name":"f","arguments":"{}","status":"incomplete"}]}`
	_, notes, err := DecodeResponseLossy([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if !anyNoteHas(notes, codec.ResponseItemStatusDropNote(1)) {
		t.Errorf("function_call 的 incomplete 状态没被计入：%v", notes)
	}
}

// 非流式：多条未完成条目 → 累计计数。
func TestItemStatusNonStreamingAccumulate(t *testing.T) {
	body := `{"id":"r1","object":"response","status":"incomplete","output":[
      {"type":"message","role":"assistant","status":"incomplete",
       "content":[{"type":"output_text","text":"a"}]},
      {"type":"function_call","call_id":"c1","name":"f","arguments":"{}","status":"in_progress"}]}`
	_, notes, err := DecodeResponseLossy([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if !anyNoteHas(notes, codec.ResponseItemStatusDropNote(2)) {
		t.Errorf("累计条目状态计数不对：%v", notes)
	}
}

// 非流式：所有条目 status=completed → 不计数（终态常态，被如实改写回 completed，
// 无丢失；计入即假阳性）。
func TestItemStatusNonStreamingCompletedClean(t *testing.T) {
	body := `{"id":"r1","object":"response","status":"completed","output":[
      {"type":"message","role":"assistant","status":"completed",
       "content":[{"type":"output_text","text":"hi"}]},
      {"type":"function_call","call_id":"c1","name":"f","arguments":"{}","status":"completed"}]}`
	_, notes, err := DecodeResponseLossy([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if anyNoteHas(notes, r67fingerprint) {
		t.Errorf("completed 条目被误报：%v", notes)
	}
}

// 非流式：条目不带 status（缺席）→ 不计数。
func TestItemStatusNonStreamingAbsentClean(t *testing.T) {
	body := `{"id":"r1","object":"response","status":"completed","output":[
      {"type":"message","role":"assistant",
       "content":[{"type":"output_text","text":"hi"}]}]}`
	_, notes, err := DecodeResponseLossy([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if anyNoteHas(notes, r67fingerprint) {
		t.Errorf("缺席 status 被误报：%v", notes)
	}
}

// 非流式：completed + incomplete 混合 → 只计未完成那条（1）。
func TestItemStatusNonStreamingMixedCountsOnlyIncomplete(t *testing.T) {
	body := `{"id":"r1","object":"response","status":"incomplete","output":[
      {"type":"message","role":"assistant","status":"completed",
       "content":[{"type":"output_text","text":"done part"}]},
      {"type":"message","role":"assistant","status":"incomplete",
       "content":[{"type":"output_text","text":"cut part"}]}]}`
	_, notes, err := DecodeResponseLossy([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if !anyNoteHas(notes, codec.ResponseItemStatusDropNote(1)) {
		t.Errorf("混合场景应只计未完成那条（1）：%v", notes)
	}
	if anyNoteHas(notes, codec.ResponseItemStatusDropNote(2)) {
		t.Errorf("completed 条目被一起计入：%v", notes)
	}
}

// 流式：output_item.done 终态帧的条目 status=incomplete → Notes() 报出，计数 1。
func TestItemStatusStreamingIncompleteNoted(t *testing.T) {
	_, notes := feedRawNotes(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","status":"in_progress"}}`,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"trunc"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","status":"incomplete","content":[{"type":"output_text","text":"trunc"}]}}`,
		`{"type":"response.incomplete","response":{"id":"r1","status":"incomplete"}}`)
	if !anyNoteHas(notes, r67fingerprint) {
		t.Fatalf("流式 incomplete 条目状态没被报出：%v", notes)
	}
	if !anyNoteHas(notes, codec.ResponseItemStatusDropNote(1)) {
		t.Errorf("want a count of exactly 1, got %v", notes)
	}
}

// 流式累计：两条 done 帧各带未完成 status → 计数 2。
func TestItemStatusStreamingAccumulate(t *testing.T) {
	_, notes := feedRawNotes(t,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","status":"incomplete","content":[{"type":"output_text","text":"a"}]}}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"c1","name":"f","arguments":"{}","status":"in_progress"}}`,
		`{"type":"response.incomplete","response":{"id":"r1","status":"incomplete"}}`)
	if !anyNoteHas(notes, codec.ResponseItemStatusDropNote(2)) {
		t.Errorf("流式累计条目状态计数不对：%v", notes)
	}
}

// 流式：done 帧条目 status=completed → 不计数。
func TestItemStatusStreamingCompletedClean(t *testing.T) {
	_, notes := feedRawNotes(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","status":"in_progress"}}`,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hi"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi"}]}}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	if anyNoteHas(notes, r67fingerprint) {
		t.Errorf("completed 条目流式被误报：%v", notes)
	}
}

// 流式去重：added 帧带 in_progress、done 帧带 incomplete → 只计一次（探针只在
// output_item.done 终态帧触发；added 帧不经 itemDone）。
func TestItemStatusStreamingNoDoubleCount(t *testing.T) {
	_, notes := feedRawNotes(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","status":"in_progress"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","status":"incomplete","content":[{"type":"output_text","text":"a"}]}}`,
		`{"type":"response.incomplete","response":{"id":"r1","status":"incomplete"}}`)
	if !anyNoteHas(notes, codec.ResponseItemStatusDropNote(1)) {
		t.Errorf("added+done 同一条目应只计 1 次：%v", notes)
	}
	if anyNoteHas(notes, codec.ResponseItemStatusDropNote(2)) {
		t.Errorf("added 的 in_progress 被重复计入：%v", notes)
	}
}

// 规则 b：同一损类流式与非流式措辞逐字一致——两条路径各解一份等价的 incomplete
// 条目，比对注记文本相同。
func TestItemStatusStreamingMatchesNonStreamingWording(t *testing.T) {
	_, streamNotes := feedRawNotes(t,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","status":"incomplete","content":[{"type":"output_text","text":"a"}]}}`,
		`{"type":"response.incomplete","response":{"id":"r1","status":"incomplete"}}`)
	body := `{"id":"r1","object":"response","status":"incomplete","output":[
      {"type":"message","role":"assistant","status":"incomplete",
       "content":[{"type":"output_text","text":"a"}]}]}`
	_, nonStreamNotes, err := DecodeResponseLossy([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	s := findNote(streamNotes, r67fingerprint)
	n := findNote(nonStreamNotes, r67fingerprint)
	if s == "" || n == "" {
		t.Fatalf("两条路径都应报出条目状态注记：stream=%v nonstream=%v", streamNotes, nonStreamNotes)
	}
	if s != n {
		t.Errorf("流式与非流式措辞不一致：\n stream=%q\n nonstream=%q", s, n)
	}
}

// 注记按整流去重：多条未完成条目只报一条带合计数的注记，不逐帧刷多条。
func TestItemStatusNoteDeduped(t *testing.T) {
	_, notes := feedRawNotes(t,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","status":"incomplete","content":[{"type":"output_text","text":"a"}]}}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"message","id":"m2","role":"assistant","status":"incomplete","content":[{"type":"output_text","text":"b"}]}}`,
		`{"type":"response.incomplete","response":{"id":"r1","status":"incomplete"}}`)
	hits := 0
	for _, x := range notes {
		if strings.Contains(x, r67fingerprint) {
			hits++
		}
	}
	if hits != 1 {
		t.Fatalf("want exactly 1 deduped item-status note, got %d (%v)", hits, notes)
	}
}
