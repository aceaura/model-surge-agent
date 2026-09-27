package chatcompletions

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次41：chat 的 usage 模态明细——prompt_tokens_details 的 image_tokens/
// text_tokens 与 completion_tokens_details 的 text_tokens（官方 openai-python
// completion_usage.py 有这三位）。此前 wirePromptDetails/wireCompletionDetails
// 不建模它们，json.Unmarshal 静默吞掉：客户端收到了这些计费/成本归因数字，
// 而本服务的 IR、流水、调度层全记零。这组测试钉住解码读入、编码回写、
// 零值不凭空造键、端到端往返，以及跨族投影时由 UsageDropDims 如实报出。

// 解码：三位模态明细各自进对应的 IR 槽。
func TestModalityTokensDecode(t *testing.T) {
	got := convertUsage(wireUsage{
		PromptTokens:     100,
		CompletionTokens: 50,
		PromptTokensDetails: &wirePromptDetails{
			ImageTokens: 40,
			TextTokens:  60,
		},
		CompletionTokensDetails: &wireCompletionDetails{
			TextTokens: 50,
		},
	})
	if got.PromptImageTokens != 40 {
		t.Errorf("prompt image_tokens 没进 IR：%d", got.PromptImageTokens)
	}
	if got.PromptTextTokens != 60 {
		t.Errorf("prompt text_tokens 没进 IR：%d", got.PromptTextTokens)
	}
	if got.CompletionTextTokens != 50 {
		t.Errorf("completion text_tokens 没进 IR：%d", got.CompletionTextTokens)
	}
}

// 编码：三位模态明细写回官方嵌套位。
func TestModalityTokensEncode(t *testing.T) {
	w := renderUsage(ir.Usage{
		InputTokens:          60,
		OutputTokens:         50,
		PromptImageTokens:    40,
		PromptTextTokens:     60,
		CompletionTextTokens: 50,
	})
	if w.PromptTokensDetails == nil {
		t.Fatalf("有模态明细却没写出 prompt_tokens_details 对象")
	}
	if w.PromptTokensDetails.ImageTokens != 40 || w.PromptTokensDetails.TextTokens != 60 {
		t.Errorf("prompt 模态明细没写对：%+v", w.PromptTokensDetails)
	}
	if w.CompletionTokensDetails == nil {
		t.Fatalf("有 completion text_tokens 却没写出 completion_tokens_details 对象")
	}
	if w.CompletionTokensDetails.TextTokens != 50 {
		t.Errorf("completion text_tokens 没写对：%+v", w.CompletionTokensDetails)
	}
}

// 只给出 prompt text_tokens 一位时，也要触发 prompt_tokens_details 对象；
// 只给出 completion text_tokens 一位时，也要触发 completion_tokens_details 对象。
func TestModalityTokensSingleDimTriggersDetails(t *testing.T) {
	w := renderUsage(ir.Usage{InputTokens: 1, PromptTextTokens: 7})
	if w.PromptTokensDetails == nil || w.PromptTokensDetails.TextTokens != 7 {
		t.Errorf("单给 prompt text_tokens 没触发嵌套对象：%+v", w.PromptTokensDetails)
	}
	w2 := renderUsage(ir.Usage{OutputTokens: 1, CompletionTextTokens: 9})
	if w2.CompletionTokensDetails == nil || w2.CompletionTokensDetails.TextTokens != 9 {
		t.Errorf("单给 completion text_tokens 没触发嵌套对象：%+v", w2.CompletionTokensDetails)
	}
}

// 零值不凭空造键：没有任何模态明细时两个嵌套对象都不出现（omitempty 语义）。
func TestModalityTokensZeroStaysAbsent(t *testing.T) {
	w := renderUsage(ir.Usage{InputTokens: 5, OutputTokens: 7})
	if w.PromptTokensDetails != nil {
		t.Errorf("无模态明细却写出了 prompt_tokens_details：%+v", w.PromptTokensDetails)
	}
	if w.CompletionTokensDetails != nil {
		t.Errorf("无模态明细却写出了 completion_tokens_details：%+v", w.CompletionTokensDetails)
	}
}

// 端到端：上游非流式响应里的模态明细，解码进 IR、编码出站可见。
func TestModalityTokensResponseRoundTrip(t *testing.T) {
	body := []byte(`{"id":"c1","object":"chat.completion","created":1700000000,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,` +
		`"prompt_tokens_details":{"image_tokens":40,"text_tokens":60},` +
		`"completion_tokens_details":{"text_tokens":50}}}`)
	resp, _, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if resp.Usage.PromptImageTokens != 40 || resp.Usage.PromptTextTokens != 60 ||
		resp.Usage.CompletionTextTokens != 50 {
		t.Fatalf("模态明细没进 IR：%+v", resp.Usage)
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	for _, want := range []string{`"image_tokens":40`, `"text_tokens":60`, `"text_tokens":50`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("出站缺 %s：%s", want, out)
		}
	}
}

// 跨族投影：chat 之外的协议没有这三位模态明细的原生槽位，UsageDropDims
// 必须如实报出——否则客户端收到的数字在换协议出站时静默蒸发。
func TestModalityTokensCrossFamilyDropNote(t *testing.T) {
	u := &ir.Usage{
		PromptImageTokens:    40,
		PromptTextTokens:     60,
		CompletionTextTokens: 50,
	}
	// 同族（chat）不报：槽位都在。
	if dims := codec.UsageDropDims(u, codec.ProtocolChatCompletions); len(dims) != 0 {
		t.Errorf("chat 出站不该报模态明细损耗，实得：%v", dims)
	}
	for _, proto := range []string{codec.ProtocolAnthropic, codec.ProtocolResponses} {
		dims := codec.UsageDropDims(u, proto)
		joined := strings.Join(dims, ",")
		for _, want := range []string{"prompt image tokens", "prompt text tokens", "completion text tokens"} {
			if !strings.Contains(joined, want) {
				t.Errorf("%s 出站漏报模态明细维度 %q，实得：%v", proto, want, dims)
			}
		}
	}
}

// 零值时跨族也不报（没有真丢弃）。
func TestModalityTokensZeroNoCrossFamilyNote(t *testing.T) {
	if dims := codec.UsageDropDims(&ir.Usage{}, codec.ProtocolAnthropic); len(dims) != 0 {
		t.Errorf("全零用量不该报任何损耗，实得：%v", dims)
	}
}
