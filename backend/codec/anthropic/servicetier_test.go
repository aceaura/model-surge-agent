package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// #23：service_tier 请求侧槽位（auto/standard_only）。OpenAI 方言 default
// 翻译成 standard_only；值集装不下的档位（priority/flex/ultrafast）不写出，
// 由 DescribeLossy 报出（措辞在 codec 层 routingloss_test 专测）。

func TestServiceTierDecodeRoundTrip(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":16,` +
		`"messages":[{"role":"user","content":"hi"}],"service_tier":"standard_only"}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.ServiceTier != "standard_only" {
		t.Fatalf("service_tier 没收下：%q", req.ServiceTier)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"service_tier":"standard_only"`) {
		t.Errorf("回写丢了 service_tier：%s", out)
	}
}

func TestServiceTierDialectTranslated(t *testing.T) {
	out, err := EncodeRequest(&ir.Request{Model: "m", MaxTokens: 16,
		Messages:    []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		ServiceTier: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"service_tier":"standard_only"`) {
		t.Errorf("default 没翻译成 standard_only：%s", out)
	}
}

func TestServiceTierUnmappableDropped(t *testing.T) {
	for _, tier := range []string{"priority", "flex", "scale", "fast", "ultrafast"} {
		out, err := EncodeRequest(&ir.Request{Model: "m", MaxTokens: 16,
			Messages:    []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
			ServiceTier: tier})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out), "service_tier") {
			t.Errorf("%s 装不下却写出去了：%s", tier, out)
		}
	}
}

func TestServiceTierAbsentStaysAbsent(t *testing.T) {
	out, err := EncodeRequest(&ir.Request{Model: "m", MaxTokens: 16,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "service_tier") {
		t.Errorf("没给的 service_tier 被凭空造出：%s", out)
	}
}
