package anthropic

import (
	"encoding/json"
	"fmt"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// DecodeRequest 把 /v1/messages 请求体解成 IR。
func DecodeRequest(body []byte) (*ir.Request, error) {
	var w wireRequest
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, ir.NewError(ir.ErrInvalidRequest, 400, "invalid_request_error",
			fmt.Sprintf("malformed request body: %v", err))
	}
	if w.Model == "" {
		return nil, ir.NewError(ir.ErrInvalidRequest, 400, "invalid_request_error", "model is required")
	}

	out := &ir.Request{
		Model:         w.Model,
		MaxTokens:     w.MaxTokens,
		Temperature:   w.Temperature,
		TopP:          w.TopP,
		TopK:          w.TopK,
		StopSequences: w.StopSequences,
		Stream:        w.Stream,
	}

	system, err := decodeContent(w.System)
	if err != nil {
		return nil, wrapField("system", err)
	}
	out.System = system

	for i, m := range w.Messages {
		content, err := decodeContent(m.Content)
		if err != nil {
			return nil, wrapField(fmt.Sprintf("messages[%d].content", i), err)
		}
		out.Messages = append(out.Messages, ir.Message{
			Role:    ir.Role(m.Role),
			Content: content,
		})
	}

	for _, t := range w.Tools {
		out.Tools = append(out.Tools, ir.Tool{
			Name:        t.Name,
			Description: t.Description,
			Schema:      string(t.InputSchema),
		})
	}
	out.ToolChoice = decodeToolChoice(w.ToolChoice)

	if w.Thinking != nil {
		out.Thinking = &ir.ThinkingConfig{
			Enabled:      w.Thinking.Type == "enabled",
			BudgetTokens: w.Thinking.BudgetTokens,
		}
	}
	if w.Metadata != nil && w.Metadata.UserID != "" {
		out.Metadata = map[string]string{"user_id": w.Metadata.UserID}
	}
	return out, nil
}

// decodeContent 认字符串与块数组两种形态。Anthropic 允许
// content 直接是字符串，等价于单个 text 块。
func decodeContent(raw json.RawMessage) ([]ir.Block, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if text == "" {
			return nil, nil
		}
		return []ir.Block{{Type: ir.BlockText, Text: text}}, nil
	}

	var blocks []wireBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, fmt.Errorf("must be a string or a block array: %w", err)
	}
	out := make([]ir.Block, 0, len(blocks))
	for _, b := range blocks {
		block, ok, err := decodeBlock(b)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, block)
		}
	}
	return out, nil
}

// decodeBlock 返回 ok=false 表示该块要丢弃（如 redacted_thinking）。
func decodeBlock(b wireBlock) (ir.Block, bool, error) {
	out := ir.Block{}
	if b.CacheControl != nil {
		out.CacheCtl = b.CacheControl.Type
	}

	switch b.Type {
	case blockText:
		out.Type = ir.BlockText
		out.Text = b.Text
	case blockImage:
		if b.Source == nil {
			return out, false, fmt.Errorf("image block needs a source")
		}
		out.Type = ir.BlockImage
		out.Image = &ir.Image{
			MediaType: b.Source.MediaType,
			Data:      b.Source.Data,
			URL:       b.Source.URL,
		}
	case blockToolUse:
		out.Type = ir.BlockToolUse
		out.ToolUse = &ir.ToolUse{ID: b.ID, Name: b.Name, Input: string(b.Input)}
	case blockToolResult:
		content, err := decodeContent(b.Content)
		if err != nil {
			return out, false, fmt.Errorf("tool_result content: %w", err)
		}
		out.Type = ir.BlockToolResult
		out.ToolResult = &ir.ToolResult{
			ToolUseID: b.ToolUseID,
			Content:   content,
			IsError:   b.IsError,
		}
	case blockThinking:
		out.Type = ir.BlockThinking
		out.Thinking = &ir.Thinking{
			Text:          b.Thinking,
			Signature:     b.Signature,
			SignatureFrom: Name,
		}
	case blockRedactedThinking:
		// 载荷是加密的，本服务无法解读也无法转成其他协议，丢弃。
		return out, false, nil
	default:
		return out, false, fmt.Errorf("unknown block type %q", b.Type)
	}
	return out, true, nil
}

func decodeToolChoice(tc *wireToolChoice) *ir.ToolChoice {
	if tc == nil {
		return nil
	}
	switch tc.Type {
	case "auto":
		return &ir.ToolChoice{Mode: ir.ToolChoiceAuto}
	case "any":
		return &ir.ToolChoice{Mode: ir.ToolChoiceAny}
	case "none":
		return &ir.ToolChoice{Mode: ir.ToolChoiceNone}
	case "tool":
		return &ir.ToolChoice{Mode: ir.ToolChoiceTool, Name: tc.Name}
	default:
		return nil
	}
}

func wrapField(field string, err error) error {
	return ir.NewError(ir.ErrInvalidRequest, 400, "invalid_request_error",
		fmt.Sprintf("%s: %v", field, err))
}
