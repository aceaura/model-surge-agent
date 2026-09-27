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

// 轮次43：responses reasoning 条目的通道来源（content 通道 reasoning_text=模型内部
// 推理原文 vs summary 通道 reasoning_summary_text=给用户看的摘要，ir.Thinking.
// ContentChannel）是 responses 一族独有维度。同族编码据此选回哪条通道、逐字往返无损；
// 投给外族时正文逐字保留（anthropic thinking / gemini thought part / chat
// reasoning_content 都写回同一段文本），但通道 provenance 无处安放、整维丢弃，此前
// 完全静默——与 message phase（轮次38）、item id 同款「内容不丢、语义标签丢」。
// 这组测试钉住跨族请求侧有损诊断报出、responses 自家静默、缺席静默、多块计数。

// channelReq 造一份带 n 个 content 通道思考块的请求（正文非空、无签名，避免触发
// 签名丢弃等其它注记，隔离出「reasoning channel」这一条）。
func channelReq(n int) *ir.Request {
	blocks := make([]ir.Block, 0, n)
	for i := 0; i < n; i++ {
		blocks = append(blocks, ir.Block{
			Type:     ir.BlockThinking,
			Thinking: &ir.Thinking{Text: "INNER", ContentChannel: true},
		})
	}
	return &ir.Request{Model: "m", MaxTokens: 10, Messages: []ir.Message{
		{Role: ir.RoleAssistant, Content: blocks},
	}}
}

func TestDiagnoseReasoningChannelDroppedOffResponses(t *testing.T) {
	req := channelReq(1)
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolAnthropic, codec.ProtocolGemini} {
		oc, _ := codec.Outbound(name)
		got := strings.Join(codec.DescribeLossy(req, name, oc.Caps()), "; ")
		if !strings.Contains(got, "content-channel marker") {
			t.Errorf("%s 应报 responses reasoning 通道丢失：%q", name, got)
		}
		// 措辞必须落在「正文保留、通道语义丢失」上，并点名两条官方通道，
		// 与流式侧（轮次15）的处置口径一致，按说明检索流水的人不会当成两种故障。
		if !strings.Contains(got, "preserved verbatim") {
			t.Errorf("%s 注记应说明正文逐字保留：%q", name, got)
		}
		if !strings.Contains(got, "reasoning_text") || !strings.Contains(got, "reasoning_summary_text") {
			t.Errorf("%s 注记应点名两条官方通道：%q", name, got)
		}
	}
	// responses 自家按标记选回原通道，逐字往返，不该误报。
	oc, _ := codec.Outbound(codec.ProtocolResponses)
	if notes := codec.DescribeLossy(req, codec.ProtocolResponses, oc.Caps()); len(notes) != 0 {
		t.Errorf("responses 自家接得住，误报：%v", notes)
	}
}

// 缺席不报：summary 通道（ContentChannel=false，历史默认）的思考块跨族不该凭空
// 多出「reasoning channel」注记——summary 通道本就没有额外 provenance 要保。
func TestNoReasoningChannelNoteWhenAbsent(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 10, Messages: []ir.Message{
		{Role: ir.RoleAssistant, Content: []ir.Block{
			{Type: ir.BlockThinking, Thinking: &ir.Thinking{Text: "S", ContentChannel: false}},
		}},
	}}
	oc, _ := codec.Outbound(codec.ProtocolChatCompletions)
	for _, n := range codec.DescribeLossy(req, codec.ProtocolChatCompletions, oc.Caps()) {
		if strings.Contains(n, "content-channel marker") {
			t.Errorf("缺席 content 通道却报了注记：%q", n)
		}
	}
}

// 多块计数：两个 content 通道思考块应报「2 reasoning block(s)」，计数与块数一致，
// 不塌成 1、也不重复报两条。
func TestReasoningChannelNoteCountsBlocks(t *testing.T) {
	req := channelReq(2)
	oc, _ := codec.Outbound(codec.ProtocolAnthropic)
	notes := codec.DescribeLossy(req, codec.ProtocolAnthropic, oc.Caps())
	var hits int
	for _, n := range notes {
		if strings.Contains(n, "content-channel marker") {
			hits++
			if !strings.Contains(n, "2 reasoning block(s)") {
				t.Errorf("计数应为 2：%q", n)
			}
		}
	}
	if hits != 1 {
		t.Errorf("应恰好报一条 reasoning channel 注记，得到 %d 条：%v", hits, notes)
	}
}
