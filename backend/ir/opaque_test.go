package ir

import (
	"encoding/json"
	"testing"
)

// D1：不透明块在 IR 内部路径上的保真。Clone 要深拷贝 Opaque 指针（改副本的
// 判别值/来路族/Item 标记不穿透原件），Body 字节按 RawMessage 不可变惯例随值共享。

func TestCloneBlocksDeepCopiesOpaque(t *testing.T) {
	body := json.RawMessage(`{"type":"code_execution_tool_result","content":"x"}`)
	src := []Block{{
		Type:   BlockOpaque,
		Opaque: &Opaque{WireType: "code_execution_tool_result", Body: body, From: "anthropic", Item: true},
	}}
	dst := cloneBlocks(src)
	if dst[0].Opaque == src[0].Opaque {
		t.Fatal("clone 复用了同一指针，不是深拷贝")
	}
	dst[0].Opaque.WireType = "mutated"
	dst[0].Opaque.From = "responses"
	dst[0].Opaque.Item = false
	if src[0].Opaque.WireType != "code_execution_tool_result" ||
		src[0].Opaque.From != "anthropic" || !src[0].Opaque.Item {
		t.Errorf("clone 穿透改到了原件: %#v", src[0].Opaque)
	}
	if string(dst[0].Opaque.Body) != string(body) {
		t.Errorf("Body 未随值保留: %s", dst[0].Opaque.Body)
	}
}
