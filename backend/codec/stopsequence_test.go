package codec_test

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次18：跨族「命中的停止序列回显值」丢弃的判据矩阵。ir.Response.StopSequence
// 只在终止原因确为 stop_sequence 时携带被命中的那条序列原文，且只有 anthropic
// 出站协议有 stop_sequence 字段能原样回吐；chat_completions / responses 都无槽位，
// 值整条蒸发。此前静默——与 usage 细分维度同理（目标无槽位、细分丢失要报出）。
// 母协议 anthropic 出站原样保留，不报；原因非 stop_sequence、或上游没给序列原文
// 时无值可丢，也不报（防假阳性）。

const stopSeqSub = "dropped the matched stop sequence"

func TestR18StopSequenceDropMatrix(t *testing.T) {
	cases := []struct {
		reason ir.StopReason
		seq    string
		name   string
		drop   bool
	}{
		// 命中且带原文，跨到无槽位的外族 → 报。
		{ir.StopStopSequence, "<|im_end|>", codec.ProtocolChatCompletions, true},
		{ir.StopStopSequence, "STOP", codec.ProtocolResponses, true},
		{ir.StopStopSequence, "STOP", codec.ProtocolGemini, true},
		// 母协议 anthropic 原样回吐 → 不报。
		{ir.StopStopSequence, "STOP", codec.ProtocolAnthropic, false},
		// 原因不是 stop_sequence：即便带着（陈旧）序列原文也无意义、编码侧不采纳 → 不报。
		{ir.StopEndTurn, "STOP", codec.ProtocolChatCompletions, false},
		{ir.StopMaxTokens, "STOP", codec.ProtocolChatCompletions, false},
		// 原因是 stop_sequence 但上游没给序列原文：无值可丢 → 不报。
		{ir.StopStopSequence, "", codec.ProtocolChatCompletions, false},
		{"", "STOP", codec.ProtocolChatCompletions, false},
	}
	for _, c := range cases {
		got := codec.DescribeResponseStopSequenceLoss(c.reason, c.seq, c.name)
		if c.drop {
			if len(got) != 1 || !strings.Contains(got[0], stopSeqSub) {
				t.Errorf("%s(seq=%q)→%s 应报丢弃一条，实得 %v", c.reason, c.seq, c.name, got)
			}
		} else if len(got) != 0 {
			t.Errorf("%s(seq=%q)→%s 不该报，实得 %v", c.reason, c.seq, c.name, got)
		}
	}
}

// 措辞必须说清「回显值丢了、只剩通用 finish/status」，并与 StopReasonFoldNote
// 的「改写终止原因」分账——命中序列的丢弃不是原因折叠，reason 侧另有良性注释。
func TestR18StopSequenceDropWording(t *testing.T) {
	got := codec.DescribeResponseStopSequenceLoss(ir.StopStopSequence, "STOP", codec.ProtocolChatCompletions)
	if len(got) != 1 {
		t.Fatalf("want 1 note, got %v", got)
	}
	for _, want := range []string{"matched stop sequence", "no field to echo", "which one matched"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("注记缺 %q：%s", want, got[0])
		}
	}
	// 不该混入 stop-reason 折叠的措辞，两者分账。
	if strings.Contains(got[0], "rewrote the stop reason") {
		t.Errorf("停止序列注记混入了原因折叠措辞：%s", got[0])
	}
}
