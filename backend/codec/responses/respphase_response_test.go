package responses

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
)

// 轮次51：responses 上游在响应的 output message 条目上给出 phase（官方
// response_output_message.phase，commentary|final_answer）。轮次38 只保全了
// **请求侧**（ir.Message.ResponsesPhase 同族 preserve-and-resend），响应侧因
// ir.Response.Content 是扁平 []Block、无消息级槽位，连 responses→responses
// 同族也带不到客户端，此前 wireRespItem 不建模 phase→json.Unmarshal 静默吞掉、
// 无注记，与请求侧 countMessagePhase 注记不对称（违反规则 c：同一损类双方都能
// 从真实流量遇到就都要报）。这组测试钉住响应解码侧两条通道（非流式整流、流式
// output_item.done 终态帧）都探测计数并经 ResponsePhaseDropNote 报出。
//
// 判据：注记里出现 ResponsePhaseDropNote 的指纹子串（"preserve-and-resend phase"，
// 与请求侧注记措辞 "the model cannot tell commentary" 刻意不同，避免混淆）。

const respPhaseFingerprint = "preserve-and-resend phase"

// 非流式：output message 带 phase → 注记报出，计数 1。
func TestRespPhaseResponseNonStreamingNoted(t *testing.T) {
	body := `{"id":"r1","object":"response","status":"completed","output":[
      {"type":"message","role":"assistant","status":"completed","phase":"final_answer",
       "content":[{"type":"output_text","text":"hi"}]}]}`
	_, notes, err := DecodeResponseLossy([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if !anyNoteHas(notes, respPhaseFingerprint) {
		t.Fatalf("非流式 phase 没被报出：%v", notes)
	}
	if !anyNoteHas(notes, codec.ResponsePhaseDropNote(1)) {
		t.Errorf("want a count of exactly 1, got %v", notes)
	}
}

// 非流式：多条 output message 各带 phase → 累计计数。
func TestRespPhaseResponseNonStreamingAccumulate(t *testing.T) {
	body := `{"id":"r1","object":"response","status":"completed","output":[
      {"type":"message","role":"assistant","status":"completed","phase":"commentary",
       "content":[{"type":"output_text","text":"thinking aloud"}]},
      {"type":"message","role":"assistant","status":"completed","phase":"final_answer",
       "content":[{"type":"output_text","text":"the answer"}]}]}`
	_, notes, err := DecodeResponseLossy([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if !anyNoteHas(notes, codec.ResponsePhaseDropNote(2)) {
		t.Errorf("累计 phase 计数不对：%v", notes)
	}
}

// 非流式：message 不带 phase → 不计数（避免假阳性，与 logprobs 注记同款纪律）。
func TestRespPhaseResponseNonStreamingAbsentClean(t *testing.T) {
	body := `{"id":"r1","object":"response","status":"completed","output":[
      {"type":"message","role":"assistant","status":"completed",
       "content":[{"type":"output_text","text":"hi"}]}]}`
	_, notes, err := DecodeResponseLossy([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if anyNoteHas(notes, respPhaseFingerprint) {
		t.Errorf("缺席 phase 被误报：%v", notes)
	}
}

// 非流式：phase 只挂 message 条目——function_call 条目上的游离 phase 不计数
// （官方 phase 仅 response_output_message 有，其余条目带它不是官方形状）。
func TestRespPhaseResponseNonMessageNotCounted(t *testing.T) {
	body := `{"id":"r1","object":"response","status":"completed","output":[
      {"type":"function_call","call_id":"c1","name":"f","arguments":"{}","phase":"final_answer"}]}`
	_, notes, err := DecodeResponseLossy([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if anyNoteHas(notes, respPhaseFingerprint) {
		t.Errorf("非 message 条目的 phase 不该计入：%v", notes)
	}
}

// 流式：output_item.done 终态帧的 message 带 phase → Notes() 报出，计数 1。
func TestRespPhaseResponseStreamingNoted(t *testing.T) {
	_, notes := feedRawNotes(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"m1","role":"assistant"}}`,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hi"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","status":"completed","phase":"final_answer","content":[{"type":"output_text","text":"hi"}]}}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	if !anyNoteHas(notes, respPhaseFingerprint) {
		t.Fatalf("流式 phase 没被报出：%v", notes)
	}
	if !anyNoteHas(notes, codec.ResponsePhaseDropNote(1)) {
		t.Errorf("want a count of exactly 1, got %v", notes)
	}
}

// 流式去重：added 与 done 两帧都带同一 message 的 phase → 只计一次（只在 done
// 终态帧计数，added 帧的 message 走 default 不处理），不重复刷计数。
func TestRespPhaseResponseStreamingNoDoubleCount(t *testing.T) {
	_, notes := feedRawNotes(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","phase":"final_answer"}}`,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hi"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","status":"completed","phase":"final_answer","content":[{"type":"output_text","text":"hi"}]}}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	if !anyNoteHas(notes, codec.ResponsePhaseDropNote(1)) {
		t.Errorf("added+done 同一 message 应只计 1 次，got %v", notes)
	}
	if anyNoteHas(notes, codec.ResponsePhaseDropNote(2)) {
		t.Errorf("added+done 被重复计成 2，got %v", notes)
	}
}

// 流式累计：两条 message 条目各自 done 带 phase → 计数 2。
func TestRespPhaseResponseStreamingAccumulate(t *testing.T) {
	_, notes := feedRawNotes(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"m1","role":"assistant"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","status":"completed","phase":"commentary","content":[{"type":"output_text","text":"a"}]}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"m2","role":"assistant"}}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"message","id":"m2","role":"assistant","status":"completed","phase":"final_answer","content":[{"type":"output_text","text":"b"}]}}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	if !anyNoteHas(notes, codec.ResponsePhaseDropNote(2)) {
		t.Errorf("流式累计 phase 计数不对：%v", notes)
	}
}

// 流式：done 帧的 message 不带 phase → 不计数。
func TestRespPhaseResponseStreamingAbsentClean(t *testing.T) {
	_, notes := feedRawNotes(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"m1","role":"assistant"}}`,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hi"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi"}]}}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	if anyNoteHas(notes, respPhaseFingerprint) {
		t.Errorf("缺席 phase 流式被误报：%v", notes)
	}
}

// 注记按整流去重：同一 message 的 phase 只报一条带合计数的注记，不逐帧刷多条。
func TestRespPhaseResponseNoteDeduped(t *testing.T) {
	_, notes := feedRawNotes(t,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","status":"completed","phase":"commentary","content":[{"type":"output_text","text":"a"}]}}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"message","id":"m2","role":"assistant","status":"completed","phase":"final_answer","content":[{"type":"output_text","text":"b"}]}}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	hits := 0
	for _, n := range notes {
		if strings.Contains(n, respPhaseFingerprint) {
			hits++
		}
	}
	if hits != 1 {
		t.Fatalf("want exactly 1 deduped phase note, got %d (%v)", hits, notes)
	}
}
