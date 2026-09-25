package codec_test

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	_ "github.com/aceaura/model-surge-agent/backend/codec/anthropic"
	_ "github.com/aceaura/model-surge-agent/backend/codec/chatcompletions"
	_ "github.com/aceaura/model-surge-agent/backend/codec/gemini"
	_ "github.com/aceaura/model-surge-agent/backend/codec/responses"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// #25：缓存断点跨族零泄漏与诊断。断点（块级 cache_control +
// tools[].cache_control + ttl）是 anthropic 专属维度，外族载荷里一个字符
// 都不能出现；丢的部分由诊断报出（工具断点报数不报值）。

func breakpointFixture() *ir.Request {
	return &ir.Request{Model: "m", MaxTokens: 100,
		System: []ir.Block{{Type: ir.BlockText, Text: "sys", CacheCtl: "ephemeral", CacheTTL: "1h"}},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{
				{Type: ir.BlockText, Text: "a", CacheCtl: "ephemeral", CacheTTL: "1h"},
				{Type: ir.BlockText, Text: "b"}, // 无断点不计数
			}},
		},
		Tools: []ir.Tool{{Name: "ping", Schema: `{"type":"object"}`,
			CacheCtl: "ephemeral", CacheTTL: "1h"}}}
}

func TestCacheBreakpointsNeverLeakToOtherFamilies(t *testing.T) {
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolGemini} {
		oc, ok := codec.Outbound(name)
		if !ok {
			t.Fatalf("outbound %q not registered", name)
		}
		out, err := oc.EncodeRequest(breakpointFixture())
		if err != nil {
			t.Fatalf("%s EncodeRequest: %v", name, err)
		}
		body := string(out)
		for _, probe := range []string{"cache_control", "ephemeral", `"1h"`} {
			if strings.Contains(body, probe) {
				t.Errorf("%s 泄漏 %q: %s", name, probe, body)
			}
		}
	}
	// anthropic 自家必须留住全部三个断点（system 折叠成一块也不丢）。
	oc, _ := codec.Outbound(codec.ProtocolAnthropic)
	out, err := oc.EncodeRequest(breakpointFixture())
	if err != nil {
		t.Fatalf("anthropic EncodeRequest: %v", err)
	}
	if n := strings.Count(string(out), `"cache_control"`); n != 3 {
		t.Errorf("anthropic 断点应全部回写（3 处），实得 %d: %s", n, out)
	}
	if n := strings.Count(string(out), `"ttl":"1h"`); n != 3 {
		t.Errorf("anthropic ttl 应全部回写（3 处），实得 %d: %s", n, out)
	}
}

func TestCacheBreakpointNotesOffAnthropic(t *testing.T) {
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolGemini} {
		got := lossyFor(t, name, breakpointFixture())
		if !strings.Contains(got, "dropped cache_control") {
			t.Errorf("%s 应报块级断点丢弃：%q", name, got)
		}
		if !strings.Contains(got, "1 tool definition(s)") {
			t.Errorf("%s 应报 1 个工具断点丢弃（报数不报值）：%q", name, got)
		}
	}
	got := lossyFor(t, codec.ProtocolAnthropic, breakpointFixture())
	if strings.Contains(got, "cache_control") || strings.Contains(got, "tool definition") {
		t.Errorf("anthropic 全接得住，不该报：%q", got)
	}
}

// 超过 anthropic 的 4 断点上限时从最靠前的开始丢（工具定义在请求体最前，
// 覆盖前缀最短）：5 个断点裁掉最早 1 个，且报出裁剪。
func TestCacheBreakpointBudgetDropsEarliestIncludingTools(t *testing.T) {
	oc, _ := codec.Outbound(codec.ProtocolAnthropic)
	shaped := breakpointFixture()
	shaped.Messages[0].Content = append(shaped.Messages[0].Content,
		ir.Block{Type: ir.BlockText, Text: "c", CacheCtl: "ephemeral"},
		ir.Block{Type: ir.BlockText, Text: "d", CacheCtl: "ephemeral"})
	notes := codec.ShapeRequest(shaped, codec.ProtocolAnthropic, oc.Caps())
	body, err := oc.EncodeRequest(shaped)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	s := string(body)
	if n := strings.Count(s, `"cache_control"`); n != 4 {
		t.Errorf("应裁到 4 个断点，实得 %d: %s", n, s)
	}
	if !strings.Contains(strings.Join(notes, "; "), "at most 4 cache breakpoints") {
		t.Errorf("裁剪未报出：%v", notes)
	}
	// 工具断点在请求体最前、覆盖前缀最短，应是被裁掉的那个。
	if strings.Contains(s, `"input_schema":{"type":"object"},"cache_control"`) {
		t.Errorf("工具断点应最先被裁: %s", s)
	}
}
