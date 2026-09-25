package gemini

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 解码侧：args 槽位来的非对象合法 JSON（数组/字符串）不能被清空成 {}——
// IR 的入参槽是字符串形态，装得下原文；清空会把一次参数损坏的调用
// 伪装成合法的无参调用。
func TestStreamDecodeNonObjectArgsKeptRaw(t *testing.T) {
	raw := `data: {"candidates":[{"content":{"role":"model","parts":[` +
		`{"functionCall":{"name":"grep","args":[1,2]}}]}}]}` + "\n\n"
	resp := aggregateStream(t, raw)
	if len(resp.Content) != 1 || resp.Content[0].ToolUse == nil {
		t.Fatalf("want 一个工具调用块，got %+v", resp.Content)
	}
	if got := resp.Content[0].ToolUse.Input; got != `[1,2]` {
		t.Errorf("非对象入参 = %q，want 原文 %q", got, `[1,2]`)
	}
}

// 编码侧：残缺/非对象入参放进 args（RawMessage 对象槽位）会让请求体
// 违反协议的对象约束；静默换成 {} 则让工具不带参数执行，是一次真实
// 副作用。规整把原文挪进 ir.RawArgsKey 键位，两条路都不走。
func TestEncodeRequestRewrapsMalformedToolInput(t *testing.T) {
	cases := []string{`{"pattern":"x`, `[1,2]`, `"str"`, `123`}
	for _, in := range cases {
		body, err := EncodeRequest(&ir.Request{
			Model: "gemini-3-pro",
			Messages: []ir.Message{{Role: ir.RoleAssistant, Content: []ir.Block{{
				Type:    ir.BlockToolUse,
				ToolUse: &ir.ToolUse{ID: "c1", Name: "grep", Input: in},
			}}}},
		})
		if err != nil {
			t.Fatalf("EncodeRequest(%q): %v", in, err)
		}
		if !json.Valid(body) {
			t.Fatalf("入参 %q 编出的请求体不是合法 JSON: %s", in, body)
		}
		s := string(body)
		if !strings.Contains(s, ir.RawArgsKey) {
			t.Errorf("入参 %q 的原文没挪进 %s: %s", in, ir.RawArgsKey, s)
		}
		if strings.Contains(s, `"args":{}`) {
			t.Errorf("入参 %q 被清空成空对象，工具会不带参数执行: %s", in, s)
		}
	}
}

// 合法对象入参字节级透传：规整不许改写正常路径。
func TestEncodeRequestKeepsValidObjectArgs(t *testing.T) {
	body, err := EncodeRequest(&ir.Request{
		Model: "gemini-3-pro",
		Messages: []ir.Message{{Role: ir.RoleAssistant, Content: []ir.Block{{
			Type:    ir.BlockToolUse,
			ToolUse: &ir.ToolUse{ID: "c1", Name: "grep", Input: `{"pattern":"TODO"}`},
		}}}},
	})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(body), `"args":{"pattern":"TODO"}`) {
		t.Errorf("合法对象入参被改写: %s", body)
	}
}
