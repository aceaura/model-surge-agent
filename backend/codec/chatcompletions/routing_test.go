package chatcompletions

import (
	"strings"
	"testing"
)

// #23：chat 目标上 service_tier 按值集映射（standard_only→default，
// ultrafast 装不下不写出），prompt_cache_key 同族往返。

func TestServiceTierMapping(t *testing.T) {
	base := `{"model":"m","messages":[{"role":"user","content":"hi"}]`
	cases := []struct {
		name, tier, want string
	}{
		{"anthropic 方言翻译", `"service_tier":"standard_only"`, `"service_tier":"default"`},
		{"本族值透传", `"service_tier":"flex"`, `"service_tier":"flex"`},
		{"responses 专属装不下", `"service_tier":"ultrafast"`, ""},
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
			s := string(out)
			if c.want == "" {
				if strings.Contains(s, "service_tier") {
					t.Errorf("装不下却写出去了：%s", s)
				}
				return
			}
			if !strings.Contains(s, c.want) {
				t.Errorf("回写丢了 %s：%s", c.want, s)
			}
		})
	}
}

func TestPromptCacheKeyRoundTrip(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"prompt_cache_key":"client-key"}`)
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
	req, err := DecodeRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
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
