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

// container 回显的跨族损耗。container 是 anthropic 专属维度，外族响应没有
// 槽位：非流式走 EncodeResponseLossy，流式走编码器 Notes()；请求侧走
// DescribeLossy。三条通道都要照实报出，anthropic 自家静默，缺席全静默。
//
// 对应旧仓 #31（2d9fc72）。

var rContainer = &ir.Container{ID: "ctr_1", ExpiresAt: "t1",
	Skills: []ir.Skill{{SkillID: "s1", Type: "custom", Version: "v1"}}}

// inbound 两族（chat_completions / responses）有客户端流式编码器；gemini 是
// 纯出站协议，没有入站编码器，故响应侧只在两族上验证。
var containerInboundForeign = []string{codec.ProtocolChatCompletions, codec.ProtocolResponses}

func TestContainerResponseNotes(t *testing.T) {
	resp := &ir.Response{ID: "m", Model: "m", Container: rContainer}
	for _, name := range containerInboundForeign {
		notes := responseLossyNotes(t, name, resp)
		if !strings.Contains(strings.Join(notes, "; "), "dropped container info") {
			t.Errorf("%s 应报 container 丢失：%v", name, notes)
		}
	}
	// anthropic 自家接得住，静默。
	if notes := responseLossyNotes(t, codec.ProtocolAnthropic, resp); len(notes) != 0 {
		t.Errorf("anthropic 误报：%v", notes)
	}
	// 无容器全静默。
	resp.Container = nil
	for _, name := range append([]string{codec.ProtocolAnthropic}, containerInboundForeign...) {
		if notes := responseLossyNotes(t, name, resp); len(notes) != 0 {
			t.Errorf("%s 无容器误报：%v", name, notes)
		}
	}
}

// responseLossyNotes 取入站编码器的非流式有损说明。
func responseLossyNotes(t *testing.T, name string, resp *ir.Response) []string {
	t.Helper()
	ic, ok := codec.Inbound(name)
	if !ok {
		t.Fatalf("inbound %q not registered", name)
	}
	le, ok := ic.(codec.LossyResponseEncoder)
	if !ok {
		t.Fatalf("inbound %q is not a LossyResponseEncoder", name)
	}
	_, notes, err := le.EncodeResponseLossy(resp)
	if err != nil {
		t.Fatalf("%s EncodeResponseLossy: %v", name, err)
	}
	return notes
}

func TestContainerStreamEncodeDropped(t *testing.T) {
	for _, name := range containerInboundForeign {
		ic, _ := codec.Inbound(name)
		// 首帧携带。
		e := ic.NewStreamEncoder(nil)
		if _, err := e.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "m", Model: "m", Container: rContainer}); err != nil {
			t.Fatalf("%s Encode start: %v", name, err)
		}
		if got := streamNotes(e); !strings.Contains(got, "dropped container info") {
			t.Errorf("%s 首帧丢 container 应报：%q", name, got)
		}
		// 晚到（EvMessageDelta 携带）同样报。
		e2 := ic.NewStreamEncoder(nil)
		e2.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "m", Model: "m"})
		e2.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Container: rContainer})
		if got := streamNotes(e2); !strings.Contains(got, "dropped container info") {
			t.Errorf("%s 晚到 container 应报：%q", name, got)
		}
		// 重复到达合并为一条。
		e3 := ic.NewStreamEncoder(nil)
		e3.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "m", Model: "m", Container: rContainer})
		e3.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Container: rContainer})
		if n := strings.Count(streamNotes(e3), "dropped container info"); n != 1 {
			t.Errorf("%s 重复到达应合并为一条，实得 %d：%q", name, n, streamNotes(e3))
		}
	}
	// anthropic 自家 encoder 不报。
	ic, _ := codec.Inbound(codec.ProtocolAnthropic)
	e := ic.NewStreamEncoder(nil)
	e.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "m", Model: "m", Container: rContainer})
	e.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn})
	if got := streamNotes(e); got != "" {
		t.Errorf("anthropic 误报：%q", got)
	}
}

func TestDiagnoseContainerDroppedOffAnthropic(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10,
		Container: &ir.Container{ID: "ctr_1", Skills: []ir.Skill{{SkillID: "s1", Type: "custom"}}}}
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolGemini} {
		oc, _ := codec.Outbound(name)
		got := strings.Join(codec.DescribeLossy(req, name, oc.Caps()), "; ")
		if !strings.Contains(got, "dropped container") {
			t.Errorf("%s 应报 container 丢失：%q", name, got)
		}
	}
	oc, _ := codec.Outbound(codec.ProtocolAnthropic)
	if notes := codec.DescribeLossy(req, codec.ProtocolAnthropic, oc.Caps()); len(notes) != 0 {
		t.Errorf("anthropic 自家接得住，误报：%v", notes)
	}
	// 缺席静默。
	req.Container = nil
	for _, name := range codec.OutboundNames() {
		oc, _ := codec.Outbound(name)
		if notes := codec.DescribeLossy(req, name, oc.Caps()); len(notes) != 0 {
			t.Errorf("%s 无 container 误报：%v", name, notes)
		}
	}
}

// streamNotes 取出流式编码器的 Notes()（可选出口），没实现则空。
func streamNotes(e codec.StreamEncoder) string {
	if sn, ok := e.(codec.StreamNotes); ok {
		return strings.Join(sn.Notes(), "; ")
	}
	return ""
}
