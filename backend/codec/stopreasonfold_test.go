package codec_test

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次17：跨族终止原因折叠的判据矩阵。context_window_exceeded 母协议是
// anthropic，max_messages / steered 母协议是 responses；跨到外族时塌进目标
// 协议的通用「输出不完整」档（chat 的 length / anthropic 的 max_tokens /
// responses 的 max_output_tokens），具体成因与它暗示的补救动作丢失，必须报出。
// 母协议出站与同族往返原样表达，不报。

const foldSub = "rewrote the stop reason"

func TestR17StopReasonFoldMatrix(t *testing.T) {
	inbound := []string{codec.ProtocolAnthropic, codec.ProtocolChatCompletions, codec.ProtocolResponses}
	cases := []struct {
		reason ir.StopReason
		name   string
		fold   bool
	}{
		{ir.StopContextWindow, codec.ProtocolAnthropic, false},
		{ir.StopContextWindow, codec.ProtocolChatCompletions, true},
		{ir.StopContextWindow, codec.ProtocolResponses, true},
		{ir.StopMaxMessages, codec.ProtocolResponses, false},
		{ir.StopMaxMessages, codec.ProtocolAnthropic, true},
		{ir.StopMaxMessages, codec.ProtocolChatCompletions, true},
		{ir.StopSteered, codec.ProtocolResponses, false},
		{ir.StopSteered, codec.ProtocolAnthropic, true},
		{ir.StopSteered, codec.ProtocolChatCompletions, true},
	}
	for _, c := range cases {
		got := codec.DescribeResponseStopReasonLoss(c.reason, c.name)
		if c.fold {
			if len(got) != 1 || !strings.Contains(got[0], foldSub) {
				t.Errorf("%s→%s 应报折叠一条，实得 %v", c.reason, c.name, got)
			}
		} else if len(got) != 0 {
			t.Errorf("%s→%s 母协议不该报，实得 %v", c.reason, c.name, got)
		}
	}
	// 普通档（含通用的 max_tokens 输出不完整档）一律不报：它们在各协议都有
	// 对应值或本就是折叠目标，报了就是噪音。
	for _, r := range []ir.StopReason{ir.StopEndTurn, ir.StopMaxTokens, ir.StopStopSequence,
		ir.StopToolUse, ir.StopContentFilter, ""} {
		for _, n := range inbound {
			if got := codec.DescribeResponseStopReasonLoss(r, n); len(got) != 0 {
				t.Errorf("普通档 %s→%s 误报：%v", r, n, got)
			}
		}
	}
}

// 折叠注记的措辞必须落在「改写了终止原因、具体成因丢失」上，并带上成因，
// 便于按说明检索流水的人判断客户端被误导的补救方向。
func TestR17StopReasonFoldWording(t *testing.T) {
	got := codec.DescribeResponseStopReasonLoss(ir.StopSteered, codec.ProtocolAnthropic)
	if len(got) != 1 {
		t.Fatalf("want 1 note, got %v", got)
	}
	for _, want := range []string{"steered", "no matching stop value", "output incomplete", "remediation"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("注记缺 %q：%s", want, got[0])
		}
	}
}
