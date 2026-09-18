package responses

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// EncodeRequest 把 IR 编成 /responses 请求体。
//
// 始终写 stream:true 与 store:false：对上游一律流式请求，由数据面按需聚合；
// 不让上游留存会话，因为换目标重试时各上游的留存状态互不可见。
func EncodeRequest(req *ir.Request) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("responses: nil request")
	}
	store := false
	w := wireRequest{
		Model:       req.Model,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      true,
		Store:       &store,
	}
	if req.MaxTokens > 0 {
		n := req.MaxTokens
		w.MaxOutputTokens = &n
	}
	// 本协议没有 stop 字段，停止序列只能丢弃。Caps 已声明不支持，
	// 需要时应通过上游模型配置的 overrides 补上。

	if len(req.System) > 0 {
		w.Instructions = joinText(req.System)
	}

	items := make([]wireItem, 0, len(req.Messages))
	for i, m := range req.Messages {
		encoded, err := encodeMessage(m)
		if err != nil {
			return nil, fmt.Errorf("responses: messages[%d]: %w", i, err)
		}
		items = append(items, encoded...)
	}
	input, err := json.Marshal(items)
	if err != nil {
		return nil, fmt.Errorf("responses: input: %w", err)
	}
	w.Input = input

	for _, t := range req.Tools {
		tool := wireTool{Type: "function", Name: t.Name, Description: t.Description}
		if t.Schema != "" {
			tool.Parameters = json.RawMessage(t.Schema)
		}
		w.Tools = append(w.Tools, tool)
	}
	choice, err := encodeToolChoice(req.ToolChoice)
	if err != nil {
		return nil, fmt.Errorf("responses: tool_choice: %w", err)
	}
	w.ToolChoice = choice

	switch {
	case req.Thinking.On():
		r := &wireReasoning{Effort: req.Thinking.Effort}
		// 只有 token 预算没有档位时（来自 Anthropic 客户端）折成档位：
		// 本协议无预算概念，不折等于把思考请求整个丢掉。
		if r.Effort == "" {
			r.Effort = effortForBudget(req.Thinking.BudgetTokens)
		}
		// 请求摘要，否则推理内容完全不可见，转回 Anthropic 时 thinking 块会是空的。
		r.Summary = "auto"
		w.Reasoning = r
	case req.Thinking.Off():
		// 关闭时不要 summary：没有推理内容可摘要，带着它是要求上游
		// 为一个不存在的过程产出摘要，属于自相矛盾的请求。
		w.Reasoning = &wireReasoning{Effort: effortNone}
	}
	if id := req.Metadata["user_id"]; id != "" {
		w.User = id
	}
	return json.Marshal(w)
}

// effortNone 是本协议表达「关闭推理」的取值，不是一个强度档位。
const effortNone = "none"

// effortForBudget 把 token 预算折成 effort 档位。
// 阈值取 Anthropic 的常见用法：1024 是最小合法预算，上万即高强度思考。
func effortForBudget(budget int) string {
	switch {
	case budget <= 0:
		return "medium"
	case budget < 4096:
		return "low"
	case budget < 16384:
		return "medium"
	default:
		return "high"
	}
}

// encodeMessage 把一条 IR 消息展开成一到多个条目。
//
// 是一对多：IR 的一条消息可能同时含文本、工具调用与推理，
// 而本协议要求它们各自成独立条目。
func encodeMessage(m ir.Message) ([]wireItem, error) {
	var (
		out            []wireItem
		parts          []wirePart
		callItems      []wireItem
		reasoningItems []wireItem
	)
	for _, b := range m.Content {
		switch b.Type {
		case ir.BlockText:
			parts = append(parts, wirePart{Type: textPartType(m.Role), Text: b.Text})
		case ir.BlockImage, ir.BlockAudio, ir.BlockDocument, ir.BlockFile:
			if b.Media == nil {
				return nil, fmt.Errorf("%s block without payload", b.Type)
			}
			part, ok := encodeMediaPart(b)
			if !ok {
				part = wirePart{Type: textPartType(m.Role), Text: codec.DowngradeMedia(b).Text}
			}
			parts = append(parts, part)
		case ir.BlockToolUse:
			if b.ToolUse == nil {
				return nil, fmt.Errorf("tool_use block without payload")
			}
			args := b.ToolUse.Input
			// arguments 必须是合法 JSON 的字符串形式；流被掐断时可能残缺，
			// 补成空对象比发语法错误的请求体更好。
			if !json.Valid([]byte(args)) {
				args = "{}"
			}
			callItems = append(callItems, wireItem{
				Type:      itemFunctionCall,
				CallID:    b.ToolUse.ID,
				Name:      b.ToolUse.Name,
				Arguments: args,
			})
		case ir.BlockToolResult:
			if b.ToolResult == nil {
				return nil, fmt.Errorf("tool_result block without payload")
			}
			// 工具结果条目要排在承载它的消息之前发出，故先落进 out。
			out = append(out, wireItem{
				Type:   itemFunctionCallOutput,
				CallID: b.ToolResult.ToolUseID,
				Output: joinText(b.ToolResult.Content),
			})
		case ir.BlockThinking:
			// Redacted 块的载荷在解码期就已舍弃，编出空 reasoning item 会被上游拒收。
			if b.Thinking == nil || b.Thinking.Redacted {
				continue
			}
			item := wireItem{Type: itemReasoning}
			if b.Thinking.Text != "" {
				item.Summary = []wireSummary{{Type: partSummaryText, Text: b.Thinking.Text}}
			}
			// 加密的推理内容只在同族协议间有效，别家的签名发过来会被拒。
			// 判定与有损诊断共用一处出处。
			if !codec.ForeignSignature(b.Thinking, Name) {
				item.EncryptedContent = b.Thinking.Signature
			}
			reasoningItems = append(reasoningItems, item)
		default:
			return nil, fmt.Errorf("cannot encode block type %q", b.Type)
		}
	}

	// 推理条目必须排在它所解释的输出之前，这是本协议的条目顺序要求。
	out = append(out, reasoningItems...)
	if len(parts) > 0 {
		content, err := json.Marshal(parts)
		if err != nil {
			return nil, err
		}
		out = append(out, wireItem{Type: itemMessage, Role: string(m.Role), Content: content})
	}
	return append(out, callItems...), nil
}

// textPartType 按角色选 part 类型：助手消息用 output_text，其余用 input_text。
// 用错会被上游按格式错误拒掉。
func textPartType(role ir.Role) string {
	if role == ir.RoleAssistant {
		return partOutputText
	}
	return partInputText
}

func joinText(blocks []ir.Block) string {
	var b strings.Builder
	for _, block := range blocks {
		if block.Type == ir.BlockText {
			b.WriteString(block.Text)
		}
	}
	return b.String()
}

// encodeMediaPart 把媒体块编成本协议的原生 part。
// 返回 ok=false 表示本协议表达不了，交由调用方降级为文本。
func encodeMediaPart(b ir.Block) (wirePart, bool) {
	media := codec.SniffMediaType(b.Media)
	// 白名单判定与有损诊断共用 Caps，避免两处漂移。
	if !(outboundCodec{}.Caps().AcceptsMedia(media)) {
		return wirePart{}, false
	}
	switch {
	case strings.HasPrefix(media, "image/"):
		return wirePart{Type: partInputImage, ImageURL: renderImageURL(b.Media)}, true

	case strings.HasPrefix(media, "audio/"):
		// input_audio 只接受内联 base64 与它认得的格式名。
		format := audioFormat(media)
		if b.Media.Data == "" || format == "" {
			return wirePart{}, false
		}
		return wirePart{Type: partInputAudio, InputAudio: &wireInputAudio{
			Data: b.Media.Data, Format: format,
		}}, true

	case b.Media.Data != "" && media != "":
		return wirePart{Type: partInputFile, Filename: b.Media.Name,
			FileData: "data:" + media + ";base64," + b.Media.Data}, true

	default:
		return wirePart{}, false
	}
}

// audioFormat 把 media type 折成本协议要的裸格式名，不认得返回空串。
func audioFormat(media string) string {
	switch media {
	case "audio/wav", "audio/x-wav":
		return "wav"
	case "audio/mpeg", "audio/mp3":
		return "mp3"
	default:
		return ""
	}
}

// renderImageURL 把 IR 的分离字段拼回本协议的单一字符串。
func renderImageURL(img *ir.Media) string {
	if img.URL != "" {
		return img.URL
	}
	// 类型嗅不出时不编造一个：谎报的类型会让上游拒收整个请求。
	if media := codec.SniffMediaType(img); media != "" {
		return "data:" + media + ";base64," + img.Data
	}
	return "data:;base64," + img.Data
}

func encodeToolChoice(tc *ir.ToolChoice) (json.RawMessage, error) {
	if tc == nil {
		return nil, nil
	}
	switch tc.Mode {
	case ir.ToolChoiceAuto:
		return json.Marshal("auto")
	case ir.ToolChoiceAny:
		return json.Marshal("required")
	case ir.ToolChoiceNone:
		return json.Marshal("none")
	case ir.ToolChoiceTool:
		return json.Marshal(map[string]string{"type": "function", "name": tc.Name})
	default:
		return nil, nil
	}
}
