package responses

import (
	"encoding/json"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// function_call_output 的 output 是 Required 键：纯媒体/空文本的工具结果
// 也必须写 ""，否则编出 {"type":"function_call_output","call_id":...} 的
// 非法形状，上游 400 拒整轮。
func TestEncodeToolResultAlwaysWritesOutputKey(t *testing.T) {
	req := &ir.Request{
		Model: "m",
		Messages: []ir.Message{{
			Role: ir.RoleUser,
			Content: []ir.Block{
				{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
					ToolUseID: "c1", Content: nil}},
			},
		}},
	}
	body, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var w struct {
		Input []struct {
			Type   string  `json:"type"`
			Output *string `json:"output"`
		} `json:"input"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(w.Input) != 1 || w.Input[0].Type != itemFunctionCallOutput {
		t.Fatalf("want single function_call_output item: %s", body)
	}
	if w.Input[0].Output == nil {
		t.Fatalf("output 键缺失（非法形状）: %s", body)
	}
	if *w.Input[0].Output != "" {
		t.Fatalf("空结果的 output 应为空串，got %q", *w.Input[0].Output)
	}
}

// 有文本结果时 output 照常写出文本。
func TestEncodeToolResultWritesTextOutput(t *testing.T) {
	req := &ir.Request{
		Model: "m",
		Messages: []ir.Message{{
			Role: ir.RoleUser,
			Content: []ir.Block{
				{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
					ToolUseID: "c1", Content: []ir.Block{{Type: ir.BlockText, Text: "ok"}}}},
			},
		}},
	}
	body, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var w struct {
		Input []struct {
			Output *string `json:"output"`
		} `json:"input"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(w.Input) != 1 || w.Input[0].Output == nil || *w.Input[0].Output != "ok" {
		t.Fatalf("output = %v: %s", w.Input, body)
	}
}
