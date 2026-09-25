package chatcompletions

import (
	"strings"
	"testing"
)

// #24：chat 顶层 verbosity 与 2026 三个请求修饰槽位（safety_identifier /
// moderation / prompt_cache_options）同族往返；不透明字段显式 null 归一。

func TestOpenAIExtrasRoundTrip(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"verbosity":"low","safety_identifier":"si-1",` +
		`"moderation":{"model":"mod","policy":{"input":"low","output":"high"}},` +
		`"prompt_cache_options":{"mode":"auto","ttl":"1h"}}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.Verbosity != "low" {
		t.Errorf("verbosity 没收下：%q", req.Verbosity)
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
		`"verbosity":"low"`,
		`"safety_identifier":"si-1"`,
		`"moderation":{"model":"mod","policy":{"input":"low","output":"high"}}`,
		`"prompt_cache_options":{"mode":"auto","ttl":"1h"}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("回写丢了 %s：%s", want, s)
		}
	}
}

// 显式 null 等同没给：留下字面量 null 会让回写多出一个上游解不动的
// null 键，跨族诊断也会误报（len>0 而值是 "null"）。
func TestOpenAIExtrasExplicitNullNormalized(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"moderation":null,"prompt_cache_options":null}`)
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
	req, err := DecodeRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"verbosity", "safety_identifier", "moderation", "prompt_cache_options"} {
		if strings.Contains(string(out), unwanted) {
			t.Errorf("没给的 %s 被凭空造出：%s", unwanted, out)
		}
	}
}
