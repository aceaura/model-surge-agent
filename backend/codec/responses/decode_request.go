package responses

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// DecodeRequest 把 /responses 请求体解成 IR。
func DecodeRequest(body []byte) (*ir.Request, error) {
	var w wireRequest
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, badRequest(fmt.Sprintf("malformed request body: %v", err))
	}
	if w.Model == "" {
		return nil, badRequest("model is required")
	}
	if stateful := statefulFields(w); len(stateful) > 0 {
		return nil, badRequest("server-side conversation state is not supported: " +
			strings.Join(stateful, ", ") + "; send the full conversation in input")
	}

	out := &ir.Request{
		Model:       w.Model,
		Temperature: w.Temperature,
		TopP:        w.TopP,
		Stream:      w.Stream,
	}
	if w.MaxOutputTokens != nil {
		out.MaxTokens = *w.MaxOutputTokens
	}
	if w.Instructions != "" {
		out.System = []ir.Block{{Type: ir.BlockText, Text: w.Instructions}}
	}

	items, err := decodeInput(w.Input)
	if err != nil {
		return nil, badRequest(fmt.Sprintf("input: %v", err))
	}
	for i, item := range items {
		if err := appendItem(out, item); err != nil {
			return nil, badRequest(fmt.Sprintf("input[%d]: %v", i, err))
		}
	}

	for _, t := range w.Tools {
		// 只认函数工具：web_search 之类的内建工具在上游侧执行，
		// 本服务无法把它们表达成 IR 的工具定义。
		//
		// 静默跳过的症状是「模型声称没有这个工具」，而客户端从响应里看不出
		// 是自己声明被丢了还是模型不愿意调，因此留一条说明。
		if t.Type != "function" {
			out.DecodeNotes = append(out.DecodeNotes, fmt.Sprintf(
				"skipped tool %q: unsupported type %q", t.Name, t.Type))
			continue
		}
		out.Tools = append(out.Tools, ir.Tool{
			Name:        t.Name,
			Description: t.Description,
			Schema:      string(t.Parameters),
		})
	}
	choice, err := decodeToolChoice(w.ToolChoice)
	if err != nil {
		return nil, badRequest(fmt.Sprintf("tool_choice: %v", err))
	}
	out.ToolChoice = choice

	out.Include = w.Include
	out.Truncation = w.Truncation
	out.ClientMetadata = w.Metadata
	out.ServiceTier = w.ServiceTier
	out.ParallelToolCalls = w.ParallelToolCalls
	out.TopLogProbs = w.TopLogProbs
	if w.Text != nil {
		out.Verbosity = w.Text.Verbosity
		out.ResponseFormat = decodeTextFormat(w.Text.Format)
	}

	if w.Reasoning != nil && w.Reasoning.Effort != "" {
		// "none" 是明确关闭，不是强度档位——同 chat_completions。
		if w.Reasoning.Effort == effortNone {
			out.Thinking = &ir.ThinkingConfig{Enabled: ir.ThinkingOff()}
		} else {
			out.Thinking = &ir.ThinkingConfig{Enabled: ir.ThinkingOn(), Effort: w.Reasoning.Effort}
		}
	}
	if w.User != "" {
		out.Metadata = map[string]string{"user_id": w.User}
	}
	return out, nil
}

// decodeInput 认字符串与条目数组两种形态。
func decodeInput(raw json.RawMessage) ([]wireItem, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if text == "" {
			return nil, nil
		}
		content, _ := json.Marshal(text)
		return []wireItem{{Type: itemMessage, Role: roleUser, Content: content}}, nil
	}
	var items []wireItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("must be a string or an item array: %w", err)
	}
	return items, nil
}

// appendItem 把一个条目并入 IR。
//
// 条目类型比消息角色更细：function_call 与 function_call_output 是独立条目，
// 而 IR 把它们当成消息里的块，所以这里要合并进相邻的消息而非各自成条。
func appendItem(out *ir.Request, item wireItem) error {
	// 缺 type 时按 role 推断：本协议允许省略 type 写成裸消息。
	kind := item.Type
	if kind == "" && item.Role != "" {
		kind = itemMessage
	}

	switch kind {
	case itemMessage:
		blocks, err := decodeContent(item.Content)
		if err != nil {
			return err
		}
		switch item.Role {
		case roleSystem, roleDeveloper:
			out.System = append(out.System, blocks...)
		case roleAssistant:
			appendBlocks(out, ir.RoleAssistant, blocks)
		default:
			appendBlocks(out, ir.RoleUser, blocks)
		}
		return nil

	case itemFunctionCall:
		appendBlocks(out, ir.RoleAssistant, []ir.Block{{
			Type: ir.BlockToolUse,
			ToolUse: &ir.ToolUse{
				ID:    item.CallID,
				Name:  item.Name,
				Input: item.Arguments,
			},
		}})
		return nil

	case itemFunctionCallOutput:
		// output 是纯字符串，无结构。
		var content []ir.Block
		if item.Output != "" {
			content = []ir.Block{{Type: ir.BlockText, Text: item.Output}}
		}
		// 本协议没有失败标记字段，失败态是我们出站时写进正文的前缀，
		// 这里认回来：不认的话换目标重试时模型会把失败当成功。
		content, isErr := codec.AdoptToolResultError(content)
		appendBlocks(out, ir.RoleUser, []ir.Block{{
			Type: ir.BlockToolResult,
			ToolResult: &ir.ToolResult{
				ToolUseID: item.CallID,
				Content:   content,
				IsError:   isErr,
			},
		}})
		return nil

	case itemReasoning:
		text := joinSummary(item.Summary)
		if text == "" && item.EncryptedContent == "" {
			return nil
		}
		appendBlocks(out, ir.RoleAssistant, []ir.Block{{
			Type: ir.BlockThinking,
			Thinking: &ir.Thinking{
				Text: text,
				// 加密的推理内容当作签名透传：语义相同（只对同族协议有效，
				// 别家无法解读），复用 SignatureFrom 就不必给 IR 加字段。
				Signature:     item.EncryptedContent,
				SignatureFrom: Name,
			},
		}})
		return nil

	default:
		return fmt.Errorf("unknown item type %q", item.Type)
	}
}

// appendBlocks 把块并进末尾消息，角色不同才新开一条。
// 本协议一个逻辑回合会拆成多个条目，逐条建消息会产出大量单块消息，
// 转成 Anthropic 时因为角色必须交替而被拒。
func appendBlocks(out *ir.Request, role ir.Role, blocks []ir.Block) {
	if len(blocks) == 0 {
		return
	}
	if n := len(out.Messages); n > 0 && out.Messages[n-1].Role == role {
		out.Messages[n-1].Content = append(out.Messages[n-1].Content, blocks...)
		return
	}
	out.Messages = append(out.Messages, ir.Message{Role: role, Content: blocks})
}

func joinSummary(items []wireSummary) string {
	var b strings.Builder
	for _, s := range items {
		b.WriteString(s.Text)
	}
	return b.String()
}

// decodeContent 认字符串与 part 数组两种形态。
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
		return nil, fmt.Errorf("content must be a string or a part array: %w", err)
	}
	out := make([]ir.Block, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case partInputText, partOutputText, "":
			out = append(out, ir.Block{Type: ir.BlockText, Text: p.Text})
		case partRefusal:
			// 拒答文本当普通文本：客户端要看到内容，且它不是错误。
			out = append(out, ir.Block{Type: ir.BlockText, Text: p.Refusal})
		case partInputImage:
			if p.ImageURL == "" {
				return nil, fmt.Errorf("input_image part needs an image_url")
			}
			media := decodeImageURL(p.ImageURL)
			media.Detail = p.Detail
			out = append(out, ir.Block{Type: ir.BlockImage, Media: media})
		case partInputAudio:
			if p.InputAudio == nil {
				return nil, fmt.Errorf("input_audio part needs a payload")
			}
			out = append(out, ir.Block{Type: ir.BlockAudio, Media: &ir.Media{
				MediaType: audioMediaType(p.InputAudio.Format),
				Data:      p.InputAudio.Data,
			}})
		case partInputFile:
			media := &ir.Media{Name: p.Filename}
			if p.FileData != "" {
				if got := decodeImageURL(p.FileData); got.Data != "" {
					media.MediaType, media.Data = got.MediaType, got.Data
				} else {
					media.URL = p.FileData
				}
			} else {
				// file_id 指向上游已存的文件，本服务不解引用，原样当 URL 带过去。
				media.URL = p.FileID
			}
			out = append(out, ir.Block{Type: codec.MediaKindFor(codec.SniffMediaType(media)), Media: media})
		case partSummaryText:
			out = append(out, ir.Block{
				Type:     ir.BlockThinking,
				Thinking: &ir.Thinking{Text: p.Text, SignatureFrom: Name},
			})
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

// decodeImageURL 拆 data URI。本协议用单个字符串同时表达内联 base64
// 与远程链接，IR 分开存，因为 Anthropic 与 Gemini 都要求分开给出。
func decodeImageURL(url string) *ir.Media {
	rest, found := strings.CutPrefix(url, "data:")
	if !found {
		return &ir.Media{URL: url}
	}
	head, payload, found := strings.Cut(rest, ",")
	if !found {
		return &ir.Media{URL: url}
	}
	media, ok := dataURIMediaType(head)
	if !ok {
		return &ir.Media{URL: url}
	}
	return &ir.Media{MediaType: media, Data: payload}
}

// dataURIMediaType 从 data URI 的头部取出 media type，并确认载荷是 base64。
//
// 参数列表可以有任意多项（RFC 2397 的 charset 之外实测还有别的），base64 恒在末位。
// 只切第一个分号的话，合法的 image/png;charset=utf-8;base64 会拿到
// "charset=utf-8;base64" 去比 "base64"，比不上，于是整段内联图片被当成远程 URL
// 塞进 Media.URL——上游去拉一个几百 KB 的伪链接，或者被降级成文本。
//
// 与 chatcompletions 那份刻意各留一份：合并要把「本协议用单个字符串同时表达内联
// 与远程」这个 wire 层细节上提到中立层，而 anthropic 与 gemini 的 wire 本来就分开
// 给出，公共层不该知道有协议把两件事合起来。两份的一致性由跨协议守卫测试顶住。
func dataURIMediaType(head string) (string, bool) {
	i := strings.LastIndex(head, ";")
	// i < 0 覆盖 data:base64,xxx 这种形态：那个 base64 占的是 media type 位，
	// 载荷并不是 base64，认下来会让出站编出一份上游解不开的载荷。
	if i < 0 || !strings.EqualFold(head[i+1:], "base64") {
		return "", false
	}
	// media type 可能为空（data:;base64,...），交给 SniffMediaType 去猜。
	if j := strings.Index(head, ";"); j >= 0 {
		return head[:j], true
	}
	return head, true
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
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("must be a string or a function object: %w", err)
	}
	if obj.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	return &ir.ToolChoice{Mode: ir.ToolChoiceTool, Name: obj.Name}, nil
}

func badRequest(msg string) error {
	return ir.NewError(ir.ErrInvalidRequest, 400, "invalid_request_error", msg)
}

// statefulFields 列出请求里命中的上游托管状态字段。
//
// 全部列出而不是命中第一个就返回：客户端往往同时带了两三个，一次只报一个
// 会让它改一处再撞一次，白等一个来回。
func statefulFields(w wireRequest) []string {
	var out []string
	if w.PreviousResponseID != "" {
		out = append(out, "previous_response_id")
	}
	if hasJSONValue(w.Conversation) {
		out = append(out, "conversation")
	}
	if hasJSONValue(w.ContextManagement) {
		out = append(out, "context_management")
	}
	if hasJSONValue(w.Prompt) {
		out = append(out, "prompt")
	}
	return out
}

// hasJSONValue 判断一个原始字段是否真的带了值：显式的 null 等于没带。
func hasJSONValue(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null"
}

// decodeTextFormat 把 text.format 解成 IR 形态。type 为 text 解成 nil：
// 那是默认形态而不是一项要求（同 chat_completions 的判据）。
func decodeTextFormat(w *wireTextFormat) *ir.ResponseFormat {
	if w == nil {
		return nil
	}
	switch w.Type {
	case "json_object":
		return &ir.ResponseFormat{Kind: ir.ResponseFormatJSON}
	case "json_schema":
		return &ir.ResponseFormat{
			Kind:   ir.ResponseFormatSchema,
			Name:   w.Name,
			Schema: string(w.Schema),
			Strict: w.Strict,
		}
	default:
		return nil
	}
}
