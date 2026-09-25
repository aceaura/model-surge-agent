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

// output 官方允许字符串或 content part 数组两种形态。声明成字符串时数组
// 形态会让 input 数组整段 Unmarshal 失败，合法请求被整单 400 拒掉。
func TestDecodeToolCallOutputArrayForm(t *testing.T) {
	body := []byte(`{"model":"m","input":[` +
		`{"type":"function_call","call_id":"c1","name":"snap","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c1","output":[` +
		`{"type":"output_text","text":"shot taken"},` +
		`{"type":"input_image","image_url":"data:image/png;base64,QUJD"}]}]}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatalf("数组形态 output 不应再整单 400：%v", err)
	}
	var tr *ir.ToolResult
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockToolResult {
				tr = b.ToolResult
			}
		}
	}
	if tr == nil || tr.ToolUseID != "c1" {
		t.Fatalf("工具结果块丢失：%+v", req.Messages)
	}
	var text string
	var img *ir.Media
	for _, b := range tr.Content {
		switch b.Type {
		case ir.BlockText:
			text += b.Text
		case ir.BlockImage:
			img = b.Media
		}
	}
	if text != "shot taken" {
		t.Errorf("数组形态的文本 part 丢失：%+v", tr.Content)
	}
	if img == nil || img.Data != "QUJD" {
		t.Errorf("数组形态的图片 part 丢失：%+v", tr.Content)
	}
}

// 字符串形态保持原样：单文本块。
func TestDecodeToolCallOutputStringForm(t *testing.T) {
	body := []byte(`{"model":"m","input":[` +
		`{"type":"function_call_output","call_id":"c1","output":"plain"}]}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	var tr *ir.ToolResult
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockToolResult {
				tr = b.ToolResult
			}
		}
	}
	if tr == nil || len(tr.Content) != 1 || tr.Content[0].Type != ir.BlockText ||
		tr.Content[0].Text != "plain" {
		t.Fatalf("字符串形态结果块变形：%+v", req.Messages)
	}
}

// output 缺省或为空也要保住一个空文本块占位：结果块内容全丢会让配平的
// tool_use 读到不存在的结果。
func TestDecodeToolCallOutputEmptyKeepsPlaceholder(t *testing.T) {
	for _, output := range []string{`"output":""`, `"output":[]`, ``} {
		item := `{"type":"function_call_output","call_id":"c1"`
		if output != "" {
			item += "," + output
		}
		body := []byte(`{"model":"m","input":[` + item + `}]}`)
		req, err := DecodeRequest(body)
		if err != nil {
			t.Fatalf("DecodeRequest(%s)：%v", output, err)
		}
		var tr *ir.ToolResult
		for _, m := range req.Messages {
			for _, b := range m.Content {
				if b.Type == ir.BlockToolResult {
					tr = b.ToolResult
				}
			}
		}
		if tr == nil || len(tr.Content) != 1 || tr.Content[0].Type != ir.BlockText {
			t.Fatalf("output=%s 时结果块 = %+v，want 单个空文本块占位", output, tr)
		}
	}
}
