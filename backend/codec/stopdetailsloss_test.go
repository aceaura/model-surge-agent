package codec_test

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	_ "github.com/aceaura/model-surge-agent/backend/codec/anthropic"
	_ "github.com/aceaura/model-surge-agent/backend/codec/chatcompletions"
	_ "github.com/aceaura/model-surge-agent/backend/codec/gemini"
	_ "github.com/aceaura/model-surge-agent/backend/codec/responses"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 拒绝档结构化分类（stop_details：策略分类 category 与解释 explanation）的跨族
// 损耗。该对象是 anthropic 专属响应槽位，只有 anthropic 解码器产出、只有 anthropic
// 出站编码器原样回写；chat_completions / responses 响应都没有 stop_details 字段，
// 跨族投影来的一律丢弃。终止原因（refusal→content_filter / incomplete）与拒绝正文
// 块各有保全/注记，独漏这层「为何被拒」的结构化归因——本组测试钉住三条通道
// （非流式 EncodeResponseLossy、流式 Notes()）都照实报出，anthropic 自家静默，
// 空对象 / 缺席全静默（规则 a：注记当且仅当真实丢弃）。
//
// 与 containerloss_test.go 互为镜像（那是 anthropic 专属容器回显被外族丢，这是
// anthropic 专属拒绝分类被外族丢），复用其 responseLossyNotes / streamNotes 助手。

// stopDetailsInboundForeign 是有客户端响应编码器的两个外族入站协议；gemini 纯出站、
// 无入站响应编码点，anthropic 是承载族（同族保全），都不在此列。
var stopDetailsInboundForeign = []string{codec.ProtocolChatCompletions, codec.ProtocolResponses}

const stopDetailsNoteFragment = "dropped the upstream refusal classification"

func TestStopDetailsResponseNotes(t *testing.T) {
	resp := &ir.Response{ID: "m", Model: "m", StopReason: ir.StopContentFilter,
		StopDetails: &ir.StopDetails{Category: "cyber", Explanation: "policy"}}
	for _, name := range stopDetailsInboundForeign {
		notes := responseLossyNotes(t, name, resp)
		if !strings.Contains(strings.Join(notes, "; "), stopDetailsNoteFragment) {
			t.Errorf("%s 应报 stop_details 丢失：%v", name, notes)
		}
	}
	// anthropic 自家接得住（原样回写 stop_details），静默。
	if notes := responseLossyNotes(t, codec.ProtocolAnthropic, resp); len(notes) != 0 {
		t.Errorf("anthropic 误报：%v", notes)
	}
}

// 只给一维（category 或 explanation）也算有归因可丢，照报。
func TestStopDetailsPartialFieldsStillNote(t *testing.T) {
	for _, tc := range []struct {
		label string
		sd    *ir.StopDetails
	}{
		{"only category", &ir.StopDetails{Category: "cyber"}},
		{"only explanation", &ir.StopDetails{Explanation: "policy"}},
	} {
		resp := &ir.Response{ID: "m", Model: "m", StopReason: ir.StopContentFilter, StopDetails: tc.sd}
		notes := responseLossyNotes(t, codec.ProtocolChatCompletions, resp)
		if !strings.Contains(strings.Join(notes, "; "), stopDetailsNoteFragment) {
			t.Errorf("%s 应报 stop_details 丢失：%v", tc.label, notes)
		}
	}
}

// 空对象（category 与 explanation 皆空，官方显式 null 与缺省同归此态）没有额外
// 归因可丢——终止原因与拒绝正文块已各自处理，故不报（杜绝空对象误报，规则 a）。
// 缺席（nil）同理全静默。
func TestStopDetailsEmptyOrNilSilent(t *testing.T) {
	for _, tc := range []struct {
		label string
		sd    *ir.StopDetails
	}{
		{"empty object", &ir.StopDetails{}},
		{"nil", nil},
	} {
		resp := &ir.Response{ID: "m", Model: "m", StopReason: ir.StopContentFilter, StopDetails: tc.sd}
		for _, name := range append([]string{codec.ProtocolAnthropic}, stopDetailsInboundForeign...) {
			if notes := responseLossyNotes(t, name, resp); len(notes) != 0 {
				t.Errorf("%s/%s 无有效 stop_details 误报：%v", name, tc.label, notes)
			}
		}
	}
}

func TestStopDetailsStreamEncodeDropped(t *testing.T) {
	for _, name := range stopDetailsInboundForeign {
		ic, _ := codec.Inbound(name)
		// 拒绝分类随收尾帧（EvMessageDelta）抵达。
		e := ic.NewStreamEncoder(nil)
		if _, err := e.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "m", Model: "m"}); err != nil {
			t.Fatalf("%s Encode start: %v", name, err)
		}
		if _, err := e.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopContentFilter,
			StopDetails: &ir.StopDetails{Category: "cyber", Explanation: "policy"}}); err != nil {
			t.Fatalf("%s Encode delta: %v", name, err)
		}
		if got := streamNotes(e); !strings.Contains(got, stopDetailsNoteFragment) {
			t.Errorf("%s 流式丢 stop_details 应报：%q", name, got)
		}

		// 重复到达合并为一条（报出即抽干）。
		e2 := ic.NewStreamEncoder(nil)
		e2.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "m", Model: "m"})
		e2.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopContentFilter,
			StopDetails: &ir.StopDetails{Category: "cyber"}})
		e2.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopContentFilter,
			StopDetails: &ir.StopDetails{Explanation: "policy"}})
		if n := strings.Count(streamNotes(e2), stopDetailsNoteFragment); n != 1 {
			t.Errorf("%s 重复到达应合并为一条，实得 %d：%q", name, n, streamNotes(e2))
		}

		// 空对象随帧抵达：无额外归因可丢，不报。
		e3 := ic.NewStreamEncoder(nil)
		e3.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "m", Model: "m"})
		e3.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopContentFilter,
			StopDetails: &ir.StopDetails{}})
		if got := streamNotes(e3); strings.Contains(got, stopDetailsNoteFragment) {
			t.Errorf("%s 空 stop_details 不应报：%q", name, got)
		}
	}

	// anthropic 自家流式编码器把 stop_details 原样回写，不报丢失。
	ic, _ := codec.Inbound(codec.ProtocolAnthropic)
	e := ic.NewStreamEncoder(nil)
	e.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "m", Model: "m"})
	e.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopContentFilter,
		StopDetails: &ir.StopDetails{Category: "cyber", Explanation: "policy"}})
	if got := streamNotes(e); strings.Contains(got, stopDetailsNoteFragment) {
		t.Errorf("anthropic 流式误报：%q", got)
	}
}

// 规则 b/c：两个外族入站协议对同一损耗必须同措辞——非流式与流式各取一条比对。
func TestStopDetailsCrossFamilyConsistentWording(t *testing.T) {
	resp := &ir.Response{ID: "m", Model: "m", StopReason: ir.StopContentFilter,
		StopDetails: &ir.StopDetails{Category: "cyber", Explanation: "policy"}}
	var nonStream []string
	for _, name := range stopDetailsInboundForeign {
		nonStream = append(nonStream, pickFragment(responseLossyNotes(t, name, resp)))
	}
	if nonStream[0] != nonStream[1] || nonStream[0] == "" {
		t.Errorf("非流式两族措辞不一致：%v", nonStream)
	}

	var stream []string
	for _, name := range stopDetailsInboundForeign {
		ic, _ := codec.Inbound(name)
		e := ic.NewStreamEncoder(nil)
		e.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "m", Model: "m"})
		e.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopContentFilter,
			StopDetails: &ir.StopDetails{Category: "cyber", Explanation: "policy"}})
		stream = append(stream, pickFragment(strings.Split(streamNotes(e), "; ")))
	}
	if stream[0] != stream[1] || stream[0] == "" {
		t.Errorf("流式两族措辞不一致：%v", stream)
	}
	// 非流式与流式同损同措辞（规则 b）。
	if nonStream[0] != stream[0] {
		t.Errorf("流式/非流式措辞不一致：非流式=%q 流式=%q", nonStream[0], stream[0])
	}
}

// pickFragment 取出含拒绝分类注记的那一条原文，便于逐字比对措辞。
func pickFragment(notes []string) string {
	for _, n := range notes {
		if strings.Contains(n, stopDetailsNoteFragment) {
			return n
		}
	}
	return ""
}
