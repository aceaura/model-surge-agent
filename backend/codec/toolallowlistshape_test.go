package codec_test

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 这个文件守的是工具白名单在整形层的落地：四个出站都没有白名单槽位，
// 收窄已声明工具是唯一的等价实现。不收窄而照原样声明，等于把客户端明令
// 禁止的工具又递了回去——模型随时可能调它，而请求与注记里都看不出异常。

func allowlistFixture(allowed []string, mode ir.ToolChoiceMode) *ir.Request {
	return &ir.Request{
		Model: "m", MaxTokens: 64,
		Messages: []ir.Message{{Role: ir.RoleUser,
			Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		Tools: []ir.Tool{
			{Name: "alpha", Schema: `{"type":"object","properties":{}}`},
			{Name: "beta", Schema: `{"type":"object","properties":{}}`},
			{Name: "gamma", Schema: `{"type":"object","properties":{}}`},
		},
		ToolChoice: &ir.ToolChoice{Mode: mode, AllowedTools: allowed},
	}
}

func toolNames(tools []ir.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}

// 收窄对四个目标一致生效，且整形层不报有损——限制通过「上游看不见别的
// 工具」等价成立。
func TestAllowlistNarrowsDeclaredToolsForEveryTarget(t *testing.T) {
	for _, out := range outboundNames() {
		req := allowlistFixture([]string{"alpha", "gamma"}, ir.ToolChoiceAuto)
		notes := codec.ShapeRequest(req, out, capsOf(t, out))
		if got := strings.Join(toolNames(req.Tools), ","); got != "alpha,gamma" {
			t.Errorf("%s: tools = %s，想要 alpha,gamma", out, got)
		}
		for _, n := range notes {
			if strings.Contains(n, "allowlist") {
				t.Errorf("%s: 收窄成功不该报注记：%v", out, notes)
			}
		}
	}
}

// any 档同样收窄，且档位保留（必须调 ∧ 只能调 alpha ⇒ 上游在收窄后的
// 列表里必须调一个）。
func TestAllowlistNarrowingKeepsAnyMode(t *testing.T) {
	req := allowlistFixture([]string{"beta"}, ir.ToolChoiceAny)
	codec.ShapeRequest(req, codec.ProtocolAnthropic, capsOf(t, codec.ProtocolAnthropic))
	if req.ToolChoice == nil || req.ToolChoice.Mode != ir.ToolChoiceAny {
		t.Fatalf("tool_choice = %+v，想要保留 any", req.ToolChoice)
	}
	if got := strings.Join(toolNames(req.Tools), ","); got != "beta" {
		t.Errorf("tools = %s，想要 beta", got)
	}
}

// 指名与禁止两档不收窄：指名调用上游本来就只调那一个，禁止调用一个都
// 不调，收窄对它们是空转。
func TestAllowlistInertOnNamedAndNoneModes(t *testing.T) {
	for _, mode := range []ir.ToolChoiceMode{ir.ToolChoiceTool, ir.ToolChoiceNone} {
		req := allowlistFixture([]string{"alpha"}, mode)
		req.ToolChoice.Name = "alpha"
		codec.ShapeRequest(req, codec.ProtocolAnthropic, capsOf(t, codec.ProtocolAnthropic))
		if len(req.Tools) != 3 {
			t.Errorf("%s: tools = %v，不该收窄", mode, toolNames(req.Tools))
		}
	}
}

// 交集全空时无从收窄：整个列表照发，必须报出。收窄到零个工具会连锁触发
// 「零工具丢 tool_choice」，那是比白名单失效大得多的破坏。
func TestUnenforceableAllowlistKeepsFullListWithNote(t *testing.T) {
	for _, out := range outboundNames() {
		req := allowlistFixture([]string{"ghost"}, ir.ToolChoiceAuto)
		notes := codec.ShapeRequest(req, out, capsOf(t, out))
		if len(req.Tools) != 3 {
			t.Errorf("%s: tools = %v，交集为空时不该收窄", out, toolNames(req.Tools))
		}
		joined := strings.Join(notes, "; ")
		if !strings.Contains(joined, "could not enforce the tool allowlist of 1 name(s)") {
			t.Errorf("%s: 未报白名单失效：%v", out, notes)
		}
	}
}

// 服务端工具恒保留且不计入交集：白名单管的是客户端声明的函数，把托管
// 搜索连带删掉是删了客户端声明过的东西。anthropic 承载服务端工具，
// 用它验证「保留」；白名单只命中服务端工具名时交集仍为空。
func TestAllowlistAlwaysKeepsServerTools(t *testing.T) {
	caps := capsOf(t, codec.ProtocolAnthropic)
	req := allowlistFixture([]string{"alpha"}, ir.ToolChoiceAuto)
	req.Tools = append(req.Tools, ir.Tool{Name: "web_search", ServerType: "web_search"})
	codec.ShapeRequest(req, codec.ProtocolAnthropic, caps)
	if got := strings.Join(toolNames(req.Tools), ","); got != "alpha,web_search" {
		t.Errorf("tools = %s，想要 alpha,web_search", got)
	}

	only := allowlistFixture([]string{"web_search"}, ir.ToolChoiceAuto)
	only.Tools = []ir.Tool{
		{Name: "alpha", Schema: `{"type":"object","properties":{}}`},
		{Name: "web_search", ServerType: "web_search"},
	}
	notes := codec.ShapeRequest(only, codec.ProtocolAnthropic, caps)
	if len(only.Tools) != 2 {
		t.Errorf("tools = %v，服务端工具不计入交集，应收窄失败", toolNames(only.Tools))
	}
	if !strings.Contains(strings.Join(notes, "; "), "could not enforce") {
		t.Errorf("未报白名单失效：%v", notes)
	}
}
