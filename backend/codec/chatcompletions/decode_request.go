package chatcompletions

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// DecodeRequest 把 /chat/completions 请求体解成 IR。
func DecodeRequest(body []byte) (*ir.Request, error) {
	var w wireRequest
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, badRequest(fmt.Sprintf("malformed request body: %v", err))
	}
	if w.Model == "" {
		return nil, badRequest("model is required")
	}

	out := &ir.Request{
		Model:       w.Model,
		Temperature: w.Temperature,
		TopP:        w.TopP,
		Stream:      w.Stream,
	}
	// max_completion_tokens 是新写法，同时出现时以它为准。
	if w.MaxCompletionTokens != nil {
		out.MaxTokens = *w.MaxCompletionTokens
	} else if w.MaxTokens != nil {
		out.MaxTokens = *w.MaxTokens
	}

	stop, err := decodeStop(w.Stop)
	if err != nil {
		return nil, badRequest(fmt.Sprintf("stop: %v", err))
	}
	out.StopSequences = stop

	for i, m := range w.Messages {
		if err := appendMessage(out, m); err != nil {
			return nil, badRequest(fmt.Sprintf("messages[%d]: %v", i, err))
		}
	}

	for _, t := range w.Tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		out.Tools = append(out.Tools, ir.Tool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Schema:      string(t.Function.Parameters),
		})
	}
	choice, err := decodeToolChoice(w.ToolChoice)
	if err != nil {
		return nil, badRequest(fmt.Sprintf("tool_choice: %v", err))
	}
	out.ToolChoice = choice

	if w.ReasoningEffort != "" {
		out.Thinking = &ir.ThinkingConfig{Enabled: true, Effort: w.ReasoningEffort}
	}
	if w.User != "" {
		out.Metadata = map[string]string{"user_id": w.User}
	}
	return out, nil
}

// appendMessage 把一条 wire 消息并入 IR。
//
// 本协议把系统提示、工具结果都表达为消息，而 IR 分别放在 System 字段与
// 消息内的块，因此这里是一对多的展开而非逐条映射。
func appendMessage(out *ir.Request, m wireMessage) error {
	switch m.Role {
	case roleSystem, roleDeveloper:
		blocks, err := decodeContent(m.Content)
		if err != nil {
			return err
		}
		out.System = append(out.System, blocks...)
		return nil

	case roleTool:
		blocks, err := decodeContent(m.Content)
		if err != nil {
			return err
		}
		// 工具结果在本协议是独立的 tool 消息，IR 里是 user 消息中的一个块。
		// 紧邻的多条 tool 消息合并进同一条 user 消息，与 Anthropic 的形态一致。
		block := ir.Block{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
			ToolUseID: m.ToolCallID,
			Content:   blocks,
		}}
		if n := len(out.Messages); n > 0 && out.Messages[n-1].Role == ir.RoleUser &&
			onlyToolResults(out.Messages[n-1].Content) {
			out.Messages[n-1].Content = append(out.Messages[n-1].Content, block)
			return nil
		}
		out.Messages = append(out.Messages, ir.Message{
			Role:    ir.RoleUser,
			Content: []ir.Block{block},
		})
		return nil

	case roleAssistant:
		blocks, err := decodeContent(m.Content)
		if err != nil {
			return err
		}
		// 推理内容排在正文之前：这是各家推理模型的实际输出顺序，
		// 转成 Anthropic 时 thinking 块也必须在 text 块之前。
		if m.ReasoningContent != "" {
			blocks = append([]ir.Block{{
				Type:     ir.BlockThinking,
				Thinking: &ir.Thinking{Text: m.ReasoningContent, SignatureFrom: Name},
			}}, blocks...)
		}
		for _, tc := range m.ToolCalls {
			blocks = append(blocks, ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: tc.Function.Arguments,
			}})
		}
		out.Messages = append(out.Messages, ir.Message{Role: ir.RoleAssistant, Content: blocks})
		return nil

	case roleUser, "":
		blocks, err := decodeContent(m.Content)
		if err != nil {
			return err
		}
		out.Messages = append(out.Messages, ir.Message{Role: ir.RoleUser, Content: blocks})
		return nil

	default:
		return fmt.Errorf("unknown role %q", m.Role)
	}
}

func onlyToolResults(blocks []ir.Block) bool {
	for _, b := range blocks {
		if b.Type != ir.BlockToolResult {
			return false
		}
	}
	return len(blocks) > 0
}

// decodeContent 认字符串、parts 数组与 null 三种形态。
func decodeContent(raw json.RawMessage) ([]ir.Block, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if text == "" {
			return nil, nil
		}
		return []ir.Block{{Type: ir.BlockText, Text: text}}, nil
	}

	var parts []wirePart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("content must be a string or a parts array: %w", err)
	}
	out := make([]ir.Block, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case partText, "":
			out = append(out, ir.Block{Type: ir.BlockText, Text: p.Text})
		case partImageURL:
			if p.ImageURL == nil {
				return nil, fmt.Errorf("image_url part needs a url")
			}
			out = append(out, ir.Block{Type: ir.BlockImage, Media: decodeImageURL(p.ImageURL.URL)})
		case partInputAudio:
			if p.InputAudio == nil {
				return nil, fmt.Errorf("input_audio part needs a payload")
			}
			out = append(out, ir.Block{Type: ir.BlockAudio, Media: &ir.Media{
				MediaType: audioMediaType(p.InputAudio.Format),
				Data:      p.InputAudio.Data,
			}})
		case partFile:
			if p.File == nil {
				return nil, fmt.Errorf("file part needs a payload")
			}
			media := &ir.Media{Name: p.File.Filename}
			if p.File.FileData != "" {
				if t, data, ok := splitDataURI(p.File.FileData); ok {
					media.MediaType, media.Data = t, data
				} else {
					media.URL = p.File.FileData
				}
			} else {
				// file_id 指向上游已存的文件，本服务不解引用，原样当 URL 带过去。
				media.URL = p.File.FileID
			}
			out = append(out, ir.Block{Type: codec.MediaKindFor(codec.SniffMediaType(media)), Media: media})
		default:
			return nil, fmt.Errorf("unknown content part type %q", p.Type)
		}
	}
	return out, nil
}

// audioMediaType 把本协议的裸格式名补成完整 media type。
// 未知格式仍加 audio/ 前缀：具体子类型不认得，但「这是音频」这个事实要保住。
func audioMediaType(format string) string {
	switch format {
	case "":
		return ""
	case "wav":
		return "audio/wav"
	case "mp3":
		return "audio/mpeg"
	default:
		return "audio/" + format
	}
}

// decodeImageURL 拆 data URI。本协议用单个 url 字段同时表达内联 base64
// 与远程链接，而 IR 分开存，因为 Anthropic 与 Gemini 都要求分开给出。
func decodeImageURL(url string) *ir.Media {
	media, data, ok := splitDataURI(url)
	if !ok {
		return &ir.Media{URL: url}
	}
	return &ir.Media{MediaType: media, Data: data}
}

func splitDataURI(url string) (media, data string, ok bool) {
	rest, found := strings.CutPrefix(url, "data:")
	if !found {
		return "", "", false
	}
	head, payload, found := strings.Cut(rest, ",")
	if !found {
		return "", "", false
	}
	media, encoding, found := strings.Cut(head, ";")
	if !found || encoding != "base64" {
		return "", "", false
	}
	return media, payload, true
}

func decodeStop(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		if one == "" {
			return nil, nil
		}
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, fmt.Errorf("must be a string or a string array: %w", err)
	}
	return many, nil
}

func decodeToolChoice(raw json.RawMessage) (*ir.ToolChoice, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var mode string
	if err := json.Unmarshal(raw, &mode); err == nil {
		switch mode {
		case "auto":
			return &ir.ToolChoice{Mode: ir.ToolChoiceAuto}, nil
		case "required":
			return &ir.ToolChoice{Mode: ir.ToolChoiceAny}, nil
		case "none":
			return &ir.ToolChoice{Mode: ir.ToolChoiceNone}, nil
		default:
			return nil, fmt.Errorf("unknown mode %q", mode)
		}
	}
	var obj wireToolChoiceObject
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("must be a string or a function object: %w", err)
	}
	if obj.Function.Name == "" {
		return nil, fmt.Errorf("function.name is required")
	}
	return &ir.ToolChoice{Mode: ir.ToolChoiceTool, Name: obj.Function.Name}, nil
}

func badRequest(msg string) error {
	return ir.NewError(ir.ErrInvalidRequest, 400, "invalid_request_error", msg)
}
