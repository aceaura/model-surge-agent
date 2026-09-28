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

// 轮次61：content 通道推理标记（ir.Thinking.ContentChannel）的**响应侧跨族**丢弃。
// 轮次43 已给请求侧（客户端历史里的 content 通道推理投给外族上游）补注记；轮次15/59
// 给了 responses **同族**响应侧（流式改标 + 投影改标）。唯独 responses 上游 → anthropic /
// chat 客户端的响应侧此前静默：正文写回 thinking / reasoning_content 逐字保留，但通道
// provenance 丢失、无注记——违反规则 c（请求侧与响应侧对同一损类都报）。
// 这组测试钉住响应侧描述符：跨族报、responses 同族门控排除、summary 通道不误报、多块计数、
// nil 安全、与流式改标注记措辞分账。

// channelResp 造一份带 n 个 content 通道思考块的响应（正文非空、无签名，隔离出
// 「content-channel marker」这一条，避免触发签名丢弃等其它注记）。
func channelResp(n int) *ir.Response {
	blocks := make([]ir.Block, 0, n)
	for i := 0; i < n; i++ {
		blocks = append(blocks, ir.Block{
			Type:     ir.BlockThinking,
			Thinking: &ir.Thinking{Text: "INNER", ContentChannel: true},
		})
	}
	return &ir.Response{ID: "r1", Model: "m", Content: blocks}
}

func TestDescribeResponseContentChannelLossFires(t *testing.T) {
	for _, name := range []string{codec.ProtocolAnthropic, codec.ProtocolChatCompletions} {
		got := codec.DescribeResponseContentChannelLoss(channelResp(1), name)
		if len(got) != 1 {
			t.Fatalf("%s：应报 1 条 content 通道丢弃注记，实得 %v", name, got)
		}
		if !strings.Contains(got[0], "content-channel marker") {
			t.Errorf("%s：注记没点名 content-channel marker：%q", name, got[0])
		}
		if !strings.Contains(got[0], "no reasoning-channel slot") {
			t.Errorf("%s：注记没说明目标协议无通道槽位：%q", name, got[0])
		}
		if !strings.Contains(got[0], "preserved verbatim") {
			t.Errorf("%s：注记没声明正文逐字保留（只丢标记）：%q", name, got[0])
		}
	}
}

// responses 同族保全通道（非流式原样回吐 content 数组），报了就是谎报，门控排除。
func TestDescribeResponseContentChannelLossGatedResponses(t *testing.T) {
	if got := codec.DescribeResponseContentChannelLoss(channelResp(2), codec.ProtocolResponses); len(got) != 0 {
		t.Errorf("responses 同族不该报 content 通道丢弃（保全通道）：%v", got)
	}
}

// summary 通道（ContentChannel=false，历史默认）不是 content 通道，不误报。
func TestDescribeResponseContentChannelLossSummaryNoFire(t *testing.T) {
	resp := &ir.Response{ID: "r1", Model: "m", Content: []ir.Block{
		{Type: ir.BlockThinking, Thinking: &ir.Thinking{Text: "SUM", ContentChannel: false}},
	}}
	for _, name := range []string{codec.ProtocolAnthropic, codec.ProtocolChatCompletions} {
		if got := codec.DescribeResponseContentChannelLoss(resp, name); len(got) != 0 {
			t.Errorf("%s：summary 通道被误报成 content 通道丢弃：%v", name, got)
		}
	}
}

func TestDescribeResponseContentChannelLossCount(t *testing.T) {
	got := codec.DescribeResponseContentChannelLoss(channelResp(3), codec.ProtocolAnthropic)
	if len(got) != 1 || !strings.Contains(got[0], "3 reasoning block(s)") {
		t.Errorf("多块计数应汇成一条报 3：%v", got)
	}
}

func TestDescribeResponseContentChannelLossNilSafe(t *testing.T) {
	if got := codec.DescribeResponseContentChannelLoss(nil, codec.ProtocolAnthropic); len(got) != 0 {
		t.Errorf("nil 响应不该 panic 也不该报：%v", got)
	}
	// 无思考块的响应同样静默。
	if got := codec.DescribeResponseContentChannelLoss(
		&ir.Response{ID: "r1", Model: "m", Content: []ir.Block{
			{Type: ir.BlockText, Text: "hi"},
		}}, codec.ProtocolChatCompletions); len(got) != 0 {
		t.Errorf("纯文本响应不该报 content 通道丢弃：%v", got)
	}
}

// 跨族注记（目标协议压根没有通道维度）与同族流式改标注记（流式事件模型无槽位）
// 是两种不同损耗，措辞必须分账、不可混用。
func TestContentChannelCrossFamilyNoteDistinctFromStreamNote(t *testing.T) {
	cross := codec.ContentChannelCrossFamilyNote(1)
	stream := codec.ContentChannelStreamNote(1)
	if cross == stream {
		t.Fatal("跨族注记与同族流式改标注记措辞雷同，无法区分两种损耗")
	}
	if !strings.Contains(cross, "no reasoning-channel slot") {
		t.Errorf("跨族注记应点明目标协议无通道槽位：%q", cross)
	}
	if !strings.Contains(stream, "streaming event model") {
		t.Errorf("流式改标注记应点明流式事件模型无槽位：%q", stream)
	}
}
