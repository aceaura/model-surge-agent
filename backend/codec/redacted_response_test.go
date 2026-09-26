package codec_test

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// D2：涂抹块（redacted_thinking）的密文只有 anthropic 同族槽位能逐字承载。
// 跨族响应编码器必须整块跳过、恰报一条有损，且既不泄漏密文、也不留下一个
// 凭空的空推理条目（那会让客户端以为模型想过了却什么都没说）。
func TestRedactedResponseIsDroppedWithNoteCrossFamily(t *testing.T) {
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses} {
		t.Run(name, func(t *testing.T) {
			ic, ok := codec.Inbound(name)
			if !ok {
				t.Fatalf("inbound %q not registered", name)
			}
			le, ok := ic.(codec.LossyResponseEncoder)
			if !ok {
				t.Fatalf("inbound %q must implement LossyResponseEncoder", name)
			}
			resp := &ir.Response{
				Content: []ir.Block{
					{Type: ir.BlockText, Text: "answer"},
					{Type: ir.BlockThinking, Thinking: &ir.Thinking{
						Redacted: true, RedactedData: "opaque", SignatureFrom: codec.ProtocolAnthropic,
					}},
				},
				StopReason: ir.StopEndTurn,
			}
			body, notes, err := le.EncodeResponseLossy(resp)
			if err != nil {
				t.Fatalf("EncodeResponseLossy: %v", err)
			}
			var hit int
			for _, n := range notes {
				if strings.Contains(n, "redacted_thinking") {
					hit++
				}
			}
			if hit != 1 {
				t.Errorf("%s 应恰报一条 redacted_thinking，实得 %v", name, notes)
			}
			if strings.Contains(string(body), "opaque") {
				t.Errorf("%s 不得把密文塞进别家槽位: %s", name, body)
			}
			if !strings.Contains(string(body), "answer") {
				t.Errorf("%s 正常文本不该被连带丢掉: %s", name, body)
			}
		})
	}
}
