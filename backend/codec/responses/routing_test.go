package responses

import (
	"strings"
	"testing"
)

// #23：responses 目标上 service_tier 值集是 chat 的超集（ultrafast 原样
// 透传，只有 anthropic 方言 standard_only 需要翻译），prompt_cache_key
// 同族往返。

func TestServiceTierMapping(t *testing.T) {
	base := `{"model":"m","input":"hi"`
	cases := []struct {
		name, tier, want string
	}{
		{"anthropic 方言翻译", `"service_tier":"standard_only"`, `"service_tier":"default"`},
		{"专属档位透传", `"service_tier":"ultrafast"`, `"service_tier":"ultrafast"`},
		{"OpenAI 方言透传", `"service_tier":"flex"`, `"service_tier":"flex"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, err := DecodeRequest([]byte(base + "," + c.tier + "}"))
			if err != nil {
				t.Fatal(err)
			}
			out, err := EncodeRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(out), c.want) {
				t.Errorf("回写丢了 %s：%s", c.want, out)
			}
		})
	}
}

func TestPromptCacheKeyRoundTrip(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","prompt_cache_key":"client-key"}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.PromptCacheKey != "client-key" {
		t.Fatalf("prompt_cache_key 没收下：%q", req.PromptCacheKey)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"prompt_cache_key":"client-key"`) {
		t.Errorf("回写丢了 prompt_cache_key：%s", out)
	}
}

func TestPromptCacheKeyAbsentStaysAbsent(t *testing.T) {
	req, err := DecodeRequest([]byte(`{"model":"m","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "prompt_cache_key") {
		t.Errorf("没给的 prompt_cache_key 被凭空造出：%s", out)
	}
}
