package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// #25：cache_control 保真补全。官方断点对象有 ttl（"5m"/"1h"，缺省 5m），
// 且 tools[] 定义上也有 cache_control 槽位。此前 ttl 与工具断点同族往返
// 都静默蒸发：1h 断点降级成 5m，计费与命中率都变。

func TestCacheControlTTLRoundTrip(t *testing.T) {
	body := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[
		{"type":"text","text":"cached prefix","cache_control":{"type":"ephemeral","ttl":"1h"}},
		{"type":"text","text":"fresh","cache_control":{"type":"ephemeral"}}]}]}`
	r, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	bs := r.Messages[0].Content
	if bs[0].CacheCtl != "ephemeral" || bs[0].CacheTTL != "1h" {
		t.Errorf("块0 断点 = %q/%q，want ephemeral/1h", bs[0].CacheCtl, bs[0].CacheTTL)
	}
	if bs[1].CacheCtl != "ephemeral" || bs[1].CacheTTL != "" {
		t.Errorf("块1 断点 = %q/%q，want ephemeral/缺省", bs[1].CacheCtl, bs[1].CacheTTL)
	}
	out, err := EncodeRequest(r)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"cache_control":{"type":"ephemeral","ttl":"1h"}`) {
		t.Errorf("1h ttl 未回写: %s", s)
	}
	if strings.Count(s, `"cache_control"`) != 2 {
		t.Errorf("断点数不对: %s", s)
	}
}

func TestToolCacheControlRoundTrip(t *testing.T) {
	body := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"ping","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral","ttl":"1h"}}]}`
	r, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if r.Tools[0].CacheCtl != "ephemeral" || r.Tools[0].CacheTTL != "1h" {
		t.Fatalf("工具断点 = %q/%q", r.Tools[0].CacheCtl, r.Tools[0].CacheTTL)
	}
	out, err := EncodeRequest(r)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(out), `"cache_control":{"type":"ephemeral","ttl":"1h"}`) {
		t.Errorf("工具断点未回写: %s", out)
	}
}

// 断点全缺省与显式 null：出站不多一个键。
func TestCacheControlAbsentStaysAbsent(t *testing.T) {
	r, err := DecodeRequest([]byte(
		`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":null}]}]}`))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if r.Messages[0].Content[0].CacheCtl != "" || r.Messages[0].Content[0].CacheTTL != "" {
		t.Errorf("显式 null 应归一为缺省：%+v", r.Messages[0].Content[0])
	}
	out, err := EncodeRequest(&ir.Request{Model: "m", MaxTokens: 10,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		Tools:    []ir.Tool{{Name: "ping", Schema: `{"type":"object"}`}}})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if strings.Contains(string(out), "cache_control") {
		t.Errorf("缺省时发明断点键: %s", out)
	}
}
