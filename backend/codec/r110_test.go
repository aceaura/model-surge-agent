package codec_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// R110（旧仓 #80 / 109ff61）：anthropic beta usage.iterations（按 message/
// compaction/advisor 迭代阶段细分的用量）只有 anthropic 有槽位。跨族出站要被
// UsageDropDims 报出来，原生形态（anthropic）必须闭嘴——报了就是谎报。
// 判据与其余 usage 细分维度同源：非零/非空值本身就是上游给过的证据。
func TestR110IterationsUsageDropDims(t *testing.T) {
	u := &ir.Usage{InputTokens: 1, OutputTokens: 1,
		Iterations: json.RawMessage(`{"message":{"input_tokens":1}}`)}
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolGemini} {
		dims := codec.UsageDropDims(u, name)
		if !strings.Contains(strings.Join(dims, ","), "usage iterations breakdown") {
			t.Errorf("%s 跨族出站没报 iterations 细分丢弃：%v", name, dims)
		}
	}
	// 原生形态闭嘴。
	if dims := codec.UsageDropDims(u, codec.ProtocolAnthropic); len(dims) != 0 {
		t.Errorf("anthropic 是 iterations 的原生槽位，不该报丢弃：%v", dims)
	}
	// 没给 iterations 时谁都不报（缺席不误报）。
	quiet := &ir.Usage{InputTokens: 1, OutputTokens: 1}
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses,
		codec.ProtocolGemini, codec.ProtocolAnthropic} {
		for _, d := range codec.UsageDropDims(quiet, name) {
			if strings.Contains(d, "iterations") {
				t.Errorf("%s 缺席误报 iterations：%v", name, d)
			}
		}
	}
}
