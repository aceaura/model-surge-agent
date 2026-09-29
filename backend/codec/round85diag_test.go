package codec_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	_ "github.com/aceaura/model-surge-agent/backend/codec/anthropic"
	_ "github.com/aceaura/model-surge-agent/backend/codec/chatcompletions"
	_ "github.com/aceaura/model-surge-agent/backend/codec/gemini"
	_ "github.com/aceaura/model-surge-agent/backend/codec/responses"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次85：anthropic 请求级 diagnostics（{previous_message_id}）的跨族损耗。它是
// anthropic 专属维度、外族无等价槽位（responses 的 prompt_cache_options.
// comparison_response_id 是类似机制但线格式与语义都不同、不互映）：请求侧走
// DescribeLossy，anthropic 自家静默、缺席全静默、外族照实报出。同族原文透传见
// anthropic/round85_test.go。与 mcp_servers/context_management 同口径
//（codec/betaparamsloss_test.go）。

// diagForeign 是三个没有请求级诊断槽位的出站协议。
var diagForeign = []string{codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolGemini}

func diagReq() *ir.Request {
	return &ir.Request{Model: "m", MaxTokens: 10,
		Messages:    []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		Diagnostics: json.RawMessage(`{"previous_message_id":"msg_abc123"}`)}
}

func TestDiagnoseDiagnosticsDroppedOffAnthropic(t *testing.T) {
	req := diagReq()
	for _, name := range diagForeign {
		oc, _ := codec.Outbound(name)
		got := strings.Join(codec.DescribeLossy(req, name, oc.Caps()), "; ")
		if !strings.Contains(got, "diagnostics") || !strings.Contains(got, "no request-level diagnostics slot") {
			t.Errorf("%s 应报 diagnostics 丢失：%q", name, got)
		}
		// 注记须点出失配归因这一具体后果，而非泛泛「dropped」。
		if !strings.Contains(got, "cache_miss_reason") {
			t.Errorf("%s 的 diagnostics 注记没说明丢失的后果（cache_miss_reason）：%q", name, got)
		}
	}
	// anthropic 自家接得住，静默（就 diagnostics 而言——其余维度不在本用例范围）。
	oc, _ := codec.Outbound(codec.ProtocolAnthropic)
	for _, n := range codec.DescribeLossy(req, codec.ProtocolAnthropic, oc.Caps()) {
		if strings.Contains(n, "diagnostics") {
			t.Errorf("anthropic 自家误报 diagnostics：%v", n)
		}
	}
}

// 缺席时全协议静默；首轮 opt-in（previous_message_id:null，外层对象非空）算「给过」，
// 跨族照报——客户端确实订阅了诊断，丢弃要留痕。
func TestDiagnoseDiagnosticsAbsentSilent(t *testing.T) {
	base := &ir.Request{Model: "m", MaxTokens: 10,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
	for _, name := range codec.OutboundNames() {
		oc, _ := codec.Outbound(name)
		for _, n := range codec.DescribeLossy(base, name, oc.Caps()) {
			if strings.Contains(n, "diagnostics") {
				t.Errorf("%s 无 diagnostics 误报：%v", name, n)
			}
		}
	}
	// 空对象 diagnostics:{} 语义等于没订阅，不该报。
	empty := diagReq()
	empty.Diagnostics = json.RawMessage(`{}`)
	for _, name := range diagForeign {
		oc, _ := codec.Outbound(name)
		for _, n := range codec.DescribeLossy(empty, name, oc.Caps()) {
			if strings.Contains(n, "diagnostics") {
				t.Errorf("%s 对空对象 diagnostics:{} 误报：%v", name, n)
			}
		}
	}
	// 首轮 opt-in {previous_message_id:null} 非空对象，跨族须报。
	optIn := diagReq()
	optIn.Diagnostics = json.RawMessage(`{"previous_message_id":null}`)
	for _, name := range diagForeign {
		oc, _ := codec.Outbound(name)
		got := strings.Join(codec.DescribeLossy(optIn, name, oc.Caps()), "; ")
		if !strings.Contains(got, "no request-level diagnostics slot") {
			t.Errorf("%s 对首轮 opt-in 的 diagnostics 漏报：%q", name, got)
		}
	}
}

// 跨族出站编码不得把 diagnostics 泄漏到外族线格式（丢弃要干净，注记负责告知）；
// 同族 anthropic 出站必须写回（透传兑现）。
func TestDiagnosticsNotWrittenToForeignWire(t *testing.T) {
	req := diagReq()
	for _, name := range diagForeign {
		out := string(encodeOut(t, name, req))
		if strings.Contains(out, "diagnostics") || strings.Contains(out, "previous_message_id") {
			t.Errorf("%s 出站泄漏 diagnostics：%s", name, out)
		}
	}
	out := string(encodeOut(t, codec.ProtocolAnthropic, req))
	if !strings.Contains(out, `"diagnostics"`) || !strings.Contains(out, "msg_abc123") {
		t.Errorf("anthropic 同族未回写 diagnostics：%s", out)
	}
}
