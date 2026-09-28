package chatcompletions

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次64（chat 客户端侧）：本协议 tool_calls 既无 anthropic 族 caller/toolset_name 槽位、
// 也无 responses 族 caller/namespace/async 槽位，故两族上游响应里工具调用的发起方
// provenance 标记投到本协议都被 encodeBlock 静默丢弃。此前请求侧报、响应侧静默（真缺口）。
// 三条响应路径（非流式、投影、真流式）同损同措辞（规则 b），两族分账各报一条。

const (
	r64anthrMarker = "caller/toolset_name provenance marker"
	r64respMarker  = "caller/namespace/async provenance marker"
)

func boolP(b bool) *bool { return &b }

func r64anthrTool() ir.Block {
	return ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
		ID: "call_a", Name: "grep", Input: `{"q":"x"}`,
		Caller:      []byte(`{"type":"code_execution_20250825"}`),
		ToolsetName: "ts_beta",
	}}
}

func r64respTool() ir.Block {
	return ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
		ID: "call_r", Name: "shell", Input: `{"cmd":"ls"}`,
		ResponsesCaller:    []byte(`{"type":"program","program":"p1"}`),
		ResponsesNamespace: "ns1",
		ResponsesAsync:     boolP(true),
	}}
}

func r64resp(blocks ...ir.Block) *ir.Response {
	return &ir.Response{ID: "r1", Model: "m", Content: blocks}
}

func r64markerNote(notes []string, marker string) string {
	for _, s := range notes {
		if strings.Contains(s, marker) {
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

// 非流式：anthropic 族标记投给 chat，报 anthropic 族那条。
func TestR64ChatNonStreamAnthropicShapeNoted(t *testing.T) {
	_, notes, err := inboundCodec{}.EncodeResponseLossy(r64resp(r64anthrTool()))
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if !anyNoteHas(notes, r64anthrMarker) {
		t.Errorf("非流式没报 anthropic 族 provenance 丢弃：%v", notes)
	}
}

// 非流式：responses 族标记投给 chat，报 responses 族那条。
func TestR64ChatNonStreamRespShapeNoted(t *testing.T) {
	_, notes, err := inboundCodec{}.EncodeResponseLossy(r64resp(r64respTool()))
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if !anyNoteHas(notes, r64respMarker) {
		t.Errorf("非流式没报 responses 族 provenance 丢弃：%v", notes)
	}
}

// 非流式：两族同现分账各报一条。
func TestR64ChatNonStreamBothShapesSplit(t *testing.T) {
	_, notes, err := inboundCodec{}.EncodeResponseLossy(r64resp(r64anthrTool(), r64respTool()))
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if !anyNoteHas(notes, r64anthrMarker) || !anyNoteHas(notes, r64respMarker) {
		t.Errorf("两族标记应分账各报一条：%v", notes)
	}
}

// 非流式：无 provenance 标记的普通工具调用不报（假阳性防线）。
func TestR64ChatNonStreamPlainToolSilent(t *testing.T) {
	plain := ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "c", Name: "f", Input: `{}`}}
	_, notes, err := inboundCodec{}.EncodeResponseLossy(r64resp(plain))
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	if anyNoteHas(notes, r64anthrMarker) || anyNoteHas(notes, r64respMarker) {
		t.Errorf("普通工具调用不该报 provenance 丢弃：%v", notes)
	}
}

// 投影路径：整份响应重放，两族骨架都带标记，流式编码器报出。
func TestR64ChatReplayNoted(t *testing.T) {
	notes := r64replayNotes(t, r64resp(r64respTool()))
	if !anyNoteHas(notes, r64respMarker) {
		t.Errorf("投影路径没报 responses 族 provenance 丢弃：%v", notes)
	}
}

// 真流式：直接喂 EvBlockStart（骨架带 Responses*），报出。
func TestR64ChatTrueStreamNoted(t *testing.T) {
	enc := newStreamEncoder()
	ev := ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
		Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
			ID: "call_s", Name: "shell",
			ResponsesCaller: []byte(`{"type":"program"}`), ResponsesAsync: boolP(true),
		}}}
	if _, err := enc.Encode(ev); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !anyNoteHas(enc.Notes(), r64respMarker) {
		t.Errorf("真流式没报 responses 族 provenance 丢弃")
	}
}

// 规则 b：非流式与投影流式两条路径措辞逐字一致（共用 ResponseTool*DropNote）。
func TestR64ChatStreamNonStreamWordingIdentical(t *testing.T) {
	_, nsNotes, err := inboundCodec{}.EncodeResponseLossy(r64resp(r64respTool()))
	if err != nil {
		t.Fatalf("EncodeResponseLossy: %v", err)
	}
	ns := r64markerNote(nsNotes, r64respMarker)
	st := r64markerNote(r64replayNotes(t, r64resp(r64respTool())), r64respMarker)
	if ns == "" || st == "" {
		t.Fatalf("两条路径都应报出：非流式=%q 流式=%q", ns, st)
	}
	if ns != st {
		t.Errorf("非流式与流式措辞不一致（违反规则 b）：\n非流式=%q\n流式=%q", ns, st)
	}
}

// Notes() 抽干式：取过一次之后不重复报。
func TestR64ChatNoteDrainedOnce(t *testing.T) {
	enc := newStreamEncoder()
	for _, ev := range ir.ResponseEvents(r64resp(r64respTool())) {
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
func TestR64ChatReplayMultipleCounted(t *testing.T) {
	notes := r64replayNotes(t, r64resp(r64respTool(), r64respTool(), r64respTool()))
	if !strings.Contains(r64markerNote(notes, r64respMarker), "3 tool call(s)") {
		t.Errorf("三块 responses 族标记应汇成一条报 3：%v", notes)
	}
}
