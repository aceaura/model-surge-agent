package anthropic

import (
	"encoding/json"
	"strings"
	"testing"
)

// anthropic 两个 beta 请求参数（mcp_servers / context_management）同族原文透传：
// 解码落进 IR 的 RawMessage、编码原样回写，缺席与显式 null 都归一为空。二者本服务
// 不解析、不改形，跨族无等价槽位由 DescribeLossy 报出（见 codec/betaparamsloss_test.go）。
// 官方 SDK 对照：messages.ts MessageCreateParams.mcp_servers（beta mcp-client）、
// .context_management（beta context-management，如 {clear_tool_uses:{...}}）。

const betaReqPrefix = `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],`

func TestBetaParamsDecodePassthrough(t *testing.T) {
	body := betaReqPrefix +
		`"mcp_servers":[{"type":"url","url":"https://x/mcp","name":"srv"}],` +
		`"context_management":{"edits":[{"type":"clear_tool_uses_20250919"}]}}`
	r, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if len(r.McpServers) == 0 {
		t.Fatal("mcp_servers 未落进 IR")
	}
	if !strings.Contains(string(r.McpServers), `"url":"https://x/mcp"`) {
		t.Errorf("mcp_servers 原文漂移：%s", r.McpServers)
	}
	if len(r.ContextManagement) == 0 {
		t.Fatal("context_management 未落进 IR")
	}
	if !strings.Contains(string(r.ContextManagement), `clear_tool_uses_20250919`) {
		t.Errorf("context_management 原文漂移：%s", r.ContextManagement)
	}
	// 必须是合法 JSON（原文透传不能破坏结构）。
	var probe any
	if err := json.Unmarshal(r.McpServers, &probe); err != nil {
		t.Errorf("mcp_servers 非合法 JSON：%v", err)
	}
	if err := json.Unmarshal(r.ContextManagement, &probe); err != nil {
		t.Errorf("context_management 非合法 JSON：%v", err)
	}
}

func TestBetaParamsDecodeAbsentAndNull(t *testing.T) {
	for _, body := range []string{
		// 完全缺席。
		`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`,
		// 显式 null（归一为没给，避免 4 字节字面量被当成配置回写）。
		betaReqPrefix + `"mcp_servers":null,"context_management":null}`,
	} {
		r, err := DecodeRequest([]byte(body))
		if err != nil {
			t.Fatalf("DecodeRequest: %v", err)
		}
		if len(r.McpServers) != 0 {
			t.Errorf("缺席/null 时 mcp_servers 应为空：%s", r.McpServers)
		}
		if len(r.ContextManagement) != 0 {
			t.Errorf("缺席/null 时 context_management 应为空：%s", r.ContextManagement)
		}
	}
}

func TestBetaParamsEncodePassthrough(t *testing.T) {
	in := betaReqPrefix +
		`"mcp_servers":[{"type":"url","url":"https://x/mcp","name":"srv"}],` +
		`"context_management":{"edits":[{"type":"clear_tool_uses_20250919"}]}}`
	r, err := DecodeRequest([]byte(in))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	out, err := EncodeRequest(r)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(out), `"mcp_servers"`) || !strings.Contains(string(out), `"url":"https://x/mcp"`) {
		t.Errorf("mcp_servers 未原样回写：%s", out)
	}
	if !strings.Contains(string(out), `"context_management"`) || !strings.Contains(string(out), `clear_tool_uses_20250919`) {
		t.Errorf("context_management 未原样回写：%s", out)
	}
}

func TestBetaParamsEncodeAbsentNoKey(t *testing.T) {
	r, err := DecodeRequest([]byte(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	out, err := EncodeRequest(r)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if strings.Contains(string(out), "mcp_servers") || strings.Contains(string(out), "context_management") {
		t.Errorf("缺席不应出键：%s", out)
	}
}

// 同族往返：decode→encode→decode 原文不漂移。
func TestBetaParamsRequestRoundTrip(t *testing.T) {
	in := betaReqPrefix +
		`"mcp_servers":[{"type":"url","url":"https://y/mcp","name":"s2"}],` +
		`"context_management":{"edits":[{"type":"clear_tool_uses_20250919","clear_at_least":2}]}}`
	r, err := DecodeRequest([]byte(in))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	out, err := EncodeRequest(r)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	back, err := DecodeRequest(out)
	if err != nil {
		t.Fatalf("DecodeRequest(往返): %v", err)
	}
	if !strings.Contains(string(back.McpServers), `"url":"https://y/mcp"`) {
		t.Errorf("往返 mcp_servers 漂移：%s", back.McpServers)
	}
	if !strings.Contains(string(back.ContextManagement), `"clear_at_least":2`) {
		t.Errorf("往返 context_management 漂移：%s", back.ContextManagement)
	}
}

// 轮次54 可达性锚点：客户端显式给「空容器」（mcp_servers:[] / context_management:{}）
// 时，解码器按原样落进 IR（len>0 且非 null，不被归一）——这正是跨族 DescribeLossy
// 曾误报「声明的 X 被丢弃」的入口（空数组/空对象语义等于什么都没声明）。此处钉住
// 解码行为，配套的注记静默判据见 codec/emptycontainerloss_test.go。
func TestBetaParamsDecodeEmptyContainerStored(t *testing.T) {
	body := betaReqPrefix + `"mcp_servers":[],"context_management":{}}`
	r, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if len(r.McpServers) == 0 {
		t.Errorf("空数组 mcp_servers:[] 应原样落进 IR（可达性前提），实得空：%q", r.McpServers)
	}
	if len(r.ContextManagement) == 0 {
		t.Errorf("空对象 context_management:{} 应原样落进 IR（可达性前提），实得空：%q", r.ContextManagement)
	}
}
