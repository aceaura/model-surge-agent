package chatcompletions

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次62：custom 工具调用（自由文本入参，ir.ToolUse.Kind==ToolCustom）投给
// chat_completions 客户端的流式路径被静默降级成 type=function。官方明确（openai-python
// discussion #2550，2026-03-28 复核）：流式 chunk 的 choices.delta.tool_calls 只支持
// type=function，custom 工具调用不进流式——这是 Chat Completions 的有意限制，非 spec
// 缺漏；而非流式 message.tool_calls 认 type=custom，故非流式编码器原样保全（本仓
// encode_stream.go EncodeResponse 早已有之）。于是流式只能降级：调用名保留、自由文本
// 入参经 EvToolInput 原样落进 function.arguments（不是 JSON、不套 {"input":…} 投影），
// custom 形态标记丢失。此前流式降级完全静默——与本仓 chat 解码器（轮次32 已读流式
// type=custom）、非流式编码器（保全）构成「输入认、非流式认、唯流式输出降级且不报」
// 的三处分叉。这组测试钉住：流式降级报出、非流式保全不报（有意不对称）、普通函数
// 调用不误报、降级确实发生（注记与处置相符）、多块计数、抽干式、措辞与请求侧分立。

// r62marker 是流式降级注记的独有子串（请求侧 CustomToolDowngradeNote 无 "in streaming"）。
const r62marker = "custom tool call(s) to function calls in streaming"

// r62customResp 造一份带 n 个 custom 工具调用块的响应（自由文本入参 + 投影并存，
// 与解码器/非流式编码器产出的 IR 形态一致）。
func r62customResp(n int) *ir.Response {
	blocks := make([]ir.Block, 0, n)
	for i := 0; i < n; i++ {
		blocks = append(blocks, ir.Block{Type: ir.BlockToolUse,
			ToolUse: &ir.ToolUse{ID: "call_1", Name: "shell", Kind: ir.ToolCustom,
				InputText: "echo hi", Input: `{"input":"echo hi"}`}})
	}
	return &ir.Response{ID: "r1", Model: "m", Content: blocks}
}

// r62replayNotes 驱动整份响应投影（ir.ResponseEvents）走流式编码器，收回注记。
// 这是「上游非流式、客户端 stream:true」这条最常见路径。
func r62replayNotes(t *testing.T, resp *ir.Response) []string {
	t.Helper()
	enc := newStreamEncoder()
	for _, ev := range ir.ResponseEvents(resp) {
		if _, err := enc.Encode(ev); err != nil {
			t.Fatalf("Encode(%v): %v", ev.Type, err)
		}
	}
	return enc.Notes()
}

// 投影路径：custom 调用经 ir.ResponseEvents 重放（splitBlock 保留骨架 Kind、自由文本
// 走 EvToolInput），流式编码器降级成 function 并报出。
func TestR62ChatReplayCustomDowngradeNoted(t *testing.T) {
	notes := r62replayNotes(t, r62customResp(1))
	if !anyNoteHas(notes, r62marker) {
		t.Errorf("投影路径没报 custom 流式降级：%v", notes)
	}
}

// 真流式路径：直接驱动 block_start（骨架带 Kind=ToolCustom，与 chat 解码器 announce /
// responses 解码器产出同款）+ 入参增量。降级确实发生（帧里是 type=function、调用名
// 保留、入参原样进 arguments），且报出注记——注记与处置相符（规则 a）。
func TestR62ChatTrueStreamCustomDowngradeNotedAndApplied(t *testing.T) {
	enc := newStreamEncoder()
	startFrames, err := enc.Encode(ir.Event{Type: ir.EvBlockStart, Index: 0,
		Block: &ir.Block{Type: ir.BlockToolUse,
			ToolUse: &ir.ToolUse{ID: "call_1", Name: "shell", Kind: ir.ToolCustom}}})
	if err != nil {
		t.Fatalf("Encode(block_start): %v", err)
	}
	start := string(startFrames[0])
	// 降级确实发生：开启帧是 type=function（不是 custom），调用名保留。
	if !strings.Contains(start, `"type":"function"`) {
		t.Errorf("custom 调用没降级成 function 开启帧：%s", start)
	}
	if strings.Contains(start, `"type":"custom"`) {
		t.Errorf("流式帧泄漏了官方不支持的 type=custom：%s", start)
	}
	if !strings.Contains(start, `"name":"shell"`) {
		t.Errorf("调用名丢失：%s", start)
	}
	inFrames, err := enc.Encode(ir.Event{Type: ir.EvToolInput, Index: 0, Text: "echo hi"})
	if err != nil {
		t.Fatalf("Encode(tool_input): %v", err)
	}
	// 自由文本入参原样进 function.arguments（不是 JSON、不套投影）。
	if !strings.Contains(string(inFrames[0]), `"arguments":"echo hi"`) {
		t.Errorf("自由文本入参没原样落进 arguments：%s", inFrames[0])
	}
	if !anyNoteHas(enc.Notes(), r62marker) {
		t.Errorf("真流式路径没报 custom 降级：%v", enc.Notes())
	}
}

// 有意不对称：非流式响应保全 type=custom（官方 message 支持），故不报降级注记。
// 这条钉住「stream:true 降级、stream:false 保全」是协议驱动的刻意分叉，不是漏报。
func TestR62ChatNonStreamPreservesCustomNoNote(t *testing.T) {
	out, notes, err := inboundCodec{}.EncodeResponseLossy(r62customResp(1))
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if !strings.Contains(string(out), `"type":"custom"`) {
		t.Errorf("非流式没保全 custom 原生形态：%s", out)
	}
	if !strings.Contains(string(out), `"custom":{"name":"shell","input":"echo hi"}`) {
		t.Errorf("非流式 custom 载荷不对：%s", out)
	}
	if anyNoteHas(notes, "to function calls") {
		t.Errorf("非流式保全却误报降级（违反规则 a）：%v", notes)
	}
}

// 普通函数调用（Kind=ToolFunction，零值）不触发降级注记——否则每个工具调用都误报。
func TestR62ChatFunctionCallNotDowngraded(t *testing.T) {
	resp := &ir.Response{ID: "r1", Model: "m", Content: []ir.Block{
		{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
			ID: "c1", Name: "get", Kind: ir.ToolFunction, Input: `{"q":1}`}}}}
	if anyNoteHas(r62replayNotes(t, resp), r62marker) {
		t.Errorf("普通函数调用被误报降级")
	}
	// 真流式同款：function 开启帧不报。
	enc := newStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvBlockStart, Index: 0,
		Block: &ir.Block{Type: ir.BlockToolUse,
			ToolUse: &ir.ToolUse{ID: "c1", Name: "get"}}}); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if anyNoteHas(enc.Notes(), r62marker) {
		t.Errorf("真流式普通函数调用被误报降级")
	}
}

// 多块投影累计计数：两个 custom 调用汇成一条报 2。
func TestR62ChatReplayMultipleCounted(t *testing.T) {
	notes := r62replayNotes(t, r62customResp(2))
	joined := strings.Join(notes, "; ")
	if !strings.Contains(joined, "downgraded 2 custom tool call(s)") {
		t.Errorf("两个 custom 调用应汇成一条报 2：%v", notes)
	}
}

// Notes() 抽干式：取过一次之后不重复报。
func TestR62ChatNoteDrainedOnce(t *testing.T) {
	enc := newStreamEncoder()
	for _, ev := range ir.ResponseEvents(r62customResp(1)) {
		if _, err := enc.Encode(ev); err != nil {
			t.Fatalf("Encode: %v", err)
		}
	}
	if first := enc.Notes(); !anyNoteHas(first, r62marker) {
		t.Fatalf("首取没报出：%v", first)
	}
	if second := enc.Notes(); anyNoteHas(second, r62marker) {
		t.Errorf("同一批被重复报出：%v", second)
	}
}

// 措辞分立：流式降级注记与请求侧 CustomToolDowngradeNote 同属 custom→function 降级类，
// 但成因（流式无 custom 槽 vs 目标协议无自由文本条目）与入参处置（原样 vs {"input":…}
// 投影）不同，措辞必须分立，否则排障时分不清是哪条路径降级。
func TestR62StreamNoteDistinctFromRequestNote(t *testing.T) {
	stream := codec.CustomToolStreamDowngradeNote(1)
	req := codec.CustomToolDowngradeNote(1)
	if stream == req {
		t.Errorf("流式与请求侧降级注记措辞相同，无法区分成因/处置")
	}
	if !strings.Contains(stream, "in streaming") {
		t.Errorf("流式注记没点明流式成因：%q", stream)
	}
	if !strings.Contains(stream, "verbatim") {
		t.Errorf("流式注记没点明入参原样进 arguments（区别于请求侧投影）：%q", stream)
	}
	if !strings.Contains(req, `wrapped as {"input"`) {
		t.Errorf("请求侧注记措辞变了（应仍描述投影包裹）：%q", req)
	}
}
