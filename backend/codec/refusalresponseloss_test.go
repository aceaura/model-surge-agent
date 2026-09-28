package codec

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次63：DescribeResponseRefusalLoss 只对没有独立 refusal 槽位的客户端协议报
// 「拒绝正文并进普通文本」；chat_completions 与 responses 有原生槽位（caps.Refusal=true），
// 原样保全、不报。判据与请求侧 countRequestRefusals 的 !caps.Refusal 门控同源（规则 c）。

func r63descriptorResp(n int) *ir.Response {
	blocks := make([]ir.Block, 0, n)
	for i := 0; i < n; i++ {
		blocks = append(blocks, ir.Block{Type: ir.BlockRefusal, Text: "no"})
	}
	return &ir.Response{ID: "r", Model: "m", Content: blocks}
}

func TestR63DescribeResponseRefusalLossAnthropicNoted(t *testing.T) {
	notes := DescribeResponseRefusalLoss(r63descriptorResp(1), ProtocolAnthropic)
	if len(notes) != 1 || !strings.Contains(notes[0], "refusal(s) from the model output") {
		t.Errorf("anthropic 应报拒绝并进文本：%v", notes)
	}
}

func TestR63DescribeResponseRefusalLossChatSilent(t *testing.T) {
	if notes := DescribeResponseRefusalLoss(r63descriptorResp(1), ProtocolChatCompletions); len(notes) != 0 {
		t.Errorf("chat 有 refusal 槽位，不应报：%v", notes)
	}
}

func TestR63DescribeResponseRefusalLossResponsesSilent(t *testing.T) {
	if notes := DescribeResponseRefusalLoss(r63descriptorResp(1), ProtocolResponses); len(notes) != 0 {
		t.Errorf("responses 有 refusal 槽位，不应报：%v", notes)
	}
}

func TestR63DescribeResponseRefusalLossNoRefusalSilent(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}
	if notes := DescribeResponseRefusalLoss(resp, ProtocolAnthropic); len(notes) != 0 {
		t.Errorf("无拒绝块不应报：%v", notes)
	}
}

func TestR63DescribeResponseRefusalLossNilResp(t *testing.T) {
	if notes := DescribeResponseRefusalLoss(nil, ProtocolAnthropic); len(notes) != 0 {
		t.Errorf("nil 响应不应报：%v", notes)
	}
}

func TestR63DescribeResponseRefusalLossMultiple(t *testing.T) {
	notes := DescribeResponseRefusalLoss(r63descriptorResp(3), ProtocolAnthropic)
	if len(notes) != 1 || !strings.Contains(notes[0], "3 refusal(s)") {
		t.Errorf("三块应汇成一条报 3：%v", notes)
	}
}

// 措辞与请求侧分账：响应侧讲「模型输出的拒绝」（客户端分不出拒答与正常正文），
// 请求侧讲「历史里的拒绝」（上游模型看不出自己上一轮拒绝过）。两条不可混用。
func TestR63ResponseRefusalNoteDistinctFromRequest(t *testing.T) {
	resp := ResponseRefusalMergeNote(1)
	if !strings.Contains(resp, "from the model output") {
		t.Errorf("响应侧措辞应点明 model output：%q", resp)
	}
	if strings.Contains(resp, "previously refused") {
		t.Errorf("响应侧措辞不应复用请求侧的 previously refused：%q", resp)
	}
}
