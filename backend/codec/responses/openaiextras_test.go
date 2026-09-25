package responses

import (
	"strings"
	"testing"
)

// #24：responses 的 safety_identifier / moderation / prompt_cache_options
// 同族往返与显式 null 归一（verbosity 挂 text 下，往返已由既有测试覆盖）。

func TestOpenAIExtrasRoundTrip(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","safety_identifier":"si-1",` +
		`"moderation":{"model":"mod","policy":{"input":"low","output":"high"}},` +
		`"prompt_cache_options":{"mode":"auto","ttl":"1h"}}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.SafetyIdentifier != "si-1" {
		t.Errorf("safety_identifier 没收下：%q", req.SafetyIdentifier)
	}
	if !strings.Contains(string(req.Moderation), `"output":"high"`) {
		t.Errorf("moderation 没收下：%s", req.Moderation)
	}
	if !strings.Contains(string(req.PromptCacheOptions), `"ttl":"1h"`) {
		t.Errorf("prompt_cache_options 没收下：%s", req.PromptCacheOptions)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		`"safety_identifier":"si-1"`,
		`"moderation":{"model":"mod","policy":{"input":"low","output":"high"}}`,
		`"prompt_cache_options":{"mode":"auto","ttl":"1h"}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("回写丢了 %s：%s", want, s)
		}
	}
}

func TestOpenAIExtrasExplicitNullNormalized(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","moderation":null,"prompt_cache_options":null}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.Moderation != nil || req.PromptCacheOptions != nil {
		t.Fatalf("显式 null 没归一：%s %s", req.Moderation, req.PromptCacheOptions)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "moderation") || strings.Contains(string(out), "prompt_cache_options") {
		t.Errorf("null 被回写成键：%s", out)
	}
}

func TestOpenAIExtrasAbsentStaysAbsent(t *testing.T) {
	req, err := DecodeRequest([]byte(`{"model":"m","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"safety_identifier", "moderation", "prompt_cache_options"} {
		if strings.Contains(string(out), unwanted) {
			t.Errorf("没给的 %s 被凭空造出：%s", unwanted, out)
		}
	}
}
