package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次64（anthropic 客户端侧）：上游响应里工具调用的发起方 provenance 标记跨族投给
// 本协议时被静默丢弃。本协议 tool_use 只有 anthropic 族 caller/toolset_name 槽位，故：
//   - responses 上游的 caller/namespace/async（ResponsesCaller/Namespace/Async）→ 本协议
//     无处落，encodeBlock 原样丢，此前无注记（真缺口）。
//   - anthropic 上游的 caller/toolset_name → 本协议同族保全，不报（假阳性防线）。
//
// 三条响应路径（非流式 EncodeResponseLossy、整份响应投影 ir.ResponseEvents→流式编码器、
// 真流式 EvBlockStart）同损同措辞（规则 b），Notes() 抽干式不重复报。

const r64respMarker = "caller/namespace/async provenance marker"

func boolP(b bool) *bool { return &b }

// r64respToolResp 造带 responses 族标记的工具调用响应（本协议会丢这层归属）。
func r64respToolResp(n int) *ir.Response {
	blocks := make([]ir.Block, 0, n)
	for i := 0; i < n; i++ {
		blocks = append(blocks, ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
			ID: "call_r", Name: "shell", Input: `{"cmd":"ls"}`,
			ResponsesCaller:    []byte(`{"type":"program","program":"p1"}`),
			ResponsesNamespace: "ns1",
			ResponsesAsync:     boolP(true),
		}})
	}
	return &ir.Response{ID: "r1", Model: "m", Content: blocks}
}

// r64anthrToolResp 造带 anthropic 族标记的工具调用响应（本协议同族保全，不报）。
func r64anthrToolResp() *ir.Response {
	return &ir.Response{ID: "r1", Model: "m", Content: []ir.Block{
		{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
			ID: "call_a", Name: "grep", Input: `{"q":"x"}`,
			Caller:      []byte(`{"type":"code_execution_20250825"}`),
			ToolsetName: "ts_beta",
		}},
	}}
}

func r64markerNote(notes []string) string {
	for _, s := range notes {
		if strings.Contains(s, r64respMarker) {
			return s
		}
	}
	return ""
}

func r64replayNotes(t *testing.T, resp *ir.Response) []string {
	t.Helper()
	enc := newStreamEncoder()
	for _, ev := range ir.ResponseEvents(resp) {
		if _, err := enc.Encode(ev); err != nil {
			t.Fatalf("Encode(%v): %v", ev.Type, err)
		}
	}
	return enc.Notes()
}

// 非流式：responses 族标记投给本协议，报出归属丢弃。
func TestR64AnthropicNonStreamRespProvenanceNoted(t *testing.T) {
	_, notes, err := inboundCodec{}.EncodeResponseLossy(r64respToolResp(1))
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if !anyNoteHas(notes, r64respMarker) {
		t.Errorf("非流式没报 responses 族 provenance 丢弃：%v", notes)
	}
}

// 非流式：anthropic 族标记同族保全，不误报。
func TestR64AnthropicNonStreamSameFamilySilent(t *testing.T) {
	_, notes, err := inboundCodec{}.EncodeResponseLossy(r64anthrToolResp())
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if anyNoteHas(notes, r64respMarker) || anyNoteHas(notes, "caller/toolset_name provenance marker") {
		t.Errorf("anthropic 族标记同族保全却被误报：%v", notes)
	}
}

// 投影路径：整份响应经 ir.ResponseEvents 重放，骨架带 Responses*，流式编码器报出。
func TestR64AnthropicReplayRespProvenanceNoted(t *testing.T) {
	notes := r64replayNotes(t, r64respToolResp(1))
	if !anyNoteHas(notes, r64respMarker) {
		t.Errorf("投影路径没报 responses 族 provenance 丢弃：%v", notes)
	}
}

// 真流式：直接喂 EvBlockStart（骨架带 Responses*），报出。
func TestR64AnthropicTrueStreamRespProvenanceNoted(t *testing.T) {
	enc := newStreamEncoder()
	ev := ir.Event{Type: ir.EvBlockStart, Index: 0,
		Block: &ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
			ID: "call_s", Name: "shell",
			ResponsesCaller:    []byte(`{"type":"program"}`),
			ResponsesNamespace: "ns",
			ResponsesAsync:     boolP(false),
		}}}
	if _, err := enc.Encode(ev); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !anyNoteHas(enc.Notes(), r64respMarker) {
		t.Errorf("真流式没报 responses 族 provenance 丢弃")
	}
}

// 规则 b：非流式与投影流式两条路径措辞逐字一致（共用 ResponseToolRespCallerDropNote）。
func TestR64AnthropicStreamNonStreamWordingIdentical(t *testing.T) {
	_, nsNotes, err := inboundCodec{}.EncodeResponseLossy(r64respToolResp(1))
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	ns := r64markerNote(nsNotes)
	st := r64markerNote(r64replayNotes(t, r64respToolResp(1)))
	if ns == "" || st == "" {
		t.Fatalf("两条路径都应报出：非流式=%q 流式=%q", ns, st)
	}
	if ns != st {
		t.Errorf("非流式与流式措辞不一致（违反规则 b）：\n非流式=%q\n流式=%q", ns, st)
	}
}

// Notes() 抽干式：取过一次之后不重复报。
func TestR64AnthropicNoteDrainedOnce(t *testing.T) {
	enc := newStreamEncoder()
	for _, ev := range ir.ResponseEvents(r64respToolResp(1)) {
		if _, err := enc.Encode(ev); err != nil {
			t.Fatalf("Encode: %v", err)
		}
	}
	if first := enc.Notes(); !anyNoteHas(first, r64respMarker) {
		t.Fatalf("首取没报出：%v", first)
	}
	if second := enc.Notes(); anyNoteHas(second, r64respMarker) {
		t.Errorf("同一批被重复报出：%v", second)
	}
}

// 多块投影累计计数。
func TestR64AnthropicReplayMultipleCounted(t *testing.T) {
	notes := r64replayNotes(t, r64respToolResp(3))
	if !strings.Contains(r64markerNote(notes), "3 tool call(s)") {
		t.Errorf("三块 responses 族标记应汇成一条报 3：%v", notes)
	}
}
