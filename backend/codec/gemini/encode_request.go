package gemini

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// EncodeRequest 把 IR 编成 generateContent 请求体。
//
// 没有 model 与 stream 字段：前者在 URL 路径里，后者由方法名决定，
// 都由 Endpoint 处理。
func EncodeRequest(req *ir.Request) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("gemini: nil request")
	}
	// 工具结果靠函数名回指，所以先建一张 id→name 表。
	// 必须整份请求扫一遍才建得全：结果块与产生它的调用块在不同消息里。
	names := toolNames(req)

	w := wireRequest{}
	if len(req.System) > 0 {
		w.SystemInstruction = &wireContent{Parts: []wirePart{{Text: joinText(req.System)}}}
	}

	// 本协议不要求 contents 首条是 user：model 起头的会话上游照收，
	// 故不像 anthropic 出站那样补占位首条消息。
	for i, m := range req.Messages {
		contents, err := encodeMessage(m, names)
		if err != nil {
			return nil, fmt.Errorf("gemini: messages[%d]: %w", i, err)
		}
		w.Contents = append(w.Contents, contents...)
	}

	if len(req.Tools) > 0 {
		decls := make([]wireFunctionDecl, 0, len(req.Tools))
		for _, t := range req.Tools {
			decl := wireFunctionDecl{Name: t.Name, Description: t.Description}
			if t.Schema != "" {
				decl.Parameters = json.RawMessage(t.Schema)
			}
			decls = append(decls, decl)
		}
		// 全部声明放进一个 tools 元素：分散成多个元素上游也认，
		// 但单元素是官方示例的形态，兼容性更稳。
		w.Tools = []wireTools{{FunctionDeclarations: decls}}
	}
	w.ToolConfig = encodeToolConfig(req.ToolChoice)

	cfg := &wireGenerateCfg{
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		TopK:          req.TopK,
		StopSequences: req.StopSequences,
	}
	if req.MaxTokens > 0 {
		n := req.MaxTokens
		cfg.MaxOutputTokens = &n
	}
	if req.Thinking != nil && req.Thinking.Enabled {
		tc := &wireThinkinCfg{IncludeThoughts: true}
		budget := req.Thinking.BudgetTokens
		// 只有 effort 没有预算时（来自 responses/chat_completions 客户端）折成预算：
		// 本协议用 token 数表达思考强度，没有档位概念。
		if budget <= 0 {
			budget = budgetForEffort(req.Thinking.Effort, req.MaxTokens)
		}
		tc.ThinkingBudget = &budget
		cfg.ThinkingConfig = tc
	}
	w.GenerationConfig = cfg

	return json.Marshal(w)
}

// budgetForEffort 把 effort 档位折成 token 预算。
// 无 max_tokens 参照时用固定档位，因为本协议的预算不要求小于输出上限。
func budgetForEffort(effort string, maxTokens int) int {
	switch effort {
	case "low", "minimal":
		return 2048
	case "high", "max":
		if maxTokens > 0 {
			return maxTokens
		}
		return 24576
	default:
		return 8192
	}
}

// toolNames 建 id→函数名 表。
//
// 本协议的 functionResponse 只有 name 没有 id，而 IR 的 tool_result 只记 id。
// 缺这张表就填不出 name，上游会因为对不上调用而拒绝整个请求。
func toolNames(req *ir.Request) map[string]string {
	out := map[string]string{}
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockToolUse && b.ToolUse != nil {
				out[b.ToolUse.ID] = b.ToolUse.Name
			}
		}
	}
	return out
}

// encodeMessage 把一条 IR 消息编成一到多个 content。
//
// 是一对多：工具结果在本协议里必须是 user 角色的独立 content，
// 而 IR 把它和别的块放在同一条消息里。
func encodeMessage(m ir.Message, names map[string]string) ([]wireContent, error) {
	var (
		out           []wireContent
		parts         []wirePart
		responseParts []wirePart
	)
	for _, b := range m.Content {
		switch b.Type {
		case ir.BlockText:
			parts = append(parts, wirePart{Text: b.Text})
		case ir.BlockImage:
			if b.Image == nil {
				return nil, fmt.Errorf("image block without payload")
			}
			if b.Image.URL != "" {
				parts = append(parts, wirePart{FileData: &wireFileData{
					MimeType: b.Image.MediaType, FileURI: b.Image.URL}})
				continue
			}
			media := b.Image.MediaType
			if media == "" {
				media = "image/png"
			}
			parts = append(parts, wirePart{InlineData: &wireBlob{MimeType: media, Data: b.Image.Data}})
		case ir.BlockThinking:
			if b.Thinking == nil || b.Thinking.Text == "" {
				continue
			}
			// 推理是带 thought 标记的 text part，不是独立的 part 类型。
			part := wirePart{Text: b.Thinking.Text, Thought: true}
			// 签名只在同族协议间有效，别家的发过来会被拒。
			if b.Thinking.SignatureFrom == Name {
				part.ThoughtSignature = b.Thinking.Signature
			}
			parts = append(parts, part)
		case ir.BlockToolUse:
			if b.ToolUse == nil {
				return nil, fmt.Errorf("tool_use block without payload")
			}
			args := b.ToolUse.Input
			// args 必须是合法 JSON 对象；流被掐断时可能残缺，
			// 补成空对象比发语法错误的请求体更好。
			if !json.Valid([]byte(args)) {
				args = "{}"
			}
			// 回指靠 name，但 id 是本协议的可选字段：带上它能让调用 id
			// 原样穿过一轮，省掉下一轮解码时的合成。
			parts = append(parts, wirePart{FunctionCall: &wireFunctionCall{
				Name: b.ToolUse.Name,
				Args: json.RawMessage(args),
				ID:   b.ToolUse.ID,
			}})
		case ir.BlockToolResult:
			if b.ToolResult == nil {
				return nil, fmt.Errorf("tool_result block without payload")
			}
			name := names[b.ToolResult.ToolUseID]
			if name == "" {
				// 对不上调用时用 id 兜底：上游大概率会拒，但比发一个
				// 空 name 更容易在日志里定位问题。
				name = b.ToolResult.ToolUseID
			}
			payload, err := wrapResponse(b.ToolResult)
			if err != nil {
				return nil, err
			}
			responseParts = append(responseParts, wirePart{FunctionResponse: &wireFunctionResp{
				Name: name, Response: payload, ID: b.ToolResult.ToolUseID,
			}})
		default:
			return nil, fmt.Errorf("cannot encode block type %q", b.Type)
		}
	}

	// 工具结果单独成一个 user content：本协议要求它与普通文本分开。
	if len(responseParts) > 0 {
		out = append(out, wireContent{Role: roleUser, Parts: responseParts})
	}
	if len(parts) > 0 {
		out = append(out, wireContent{Role: roleFor(m.Role), Parts: parts})
	}
	return out, nil
}

// wrapResponse 把工具结果包成 response 要求的对象形态。
// 本协议不接受裸字符串，必须是对象。
func wrapResponse(result *ir.ToolResult) (json.RawMessage, error) {
	text := joinText(result.Content)
	key := "output"
	if result.IsError {
		key = "error"
	}
	return json.Marshal(map[string]string{key: text})
}

// roleFor 把 IR 角色映射成本协议的取值：assistant 在这里叫 model。
func roleFor(role ir.Role) string {
	if role == ir.RoleAssistant {
		return roleModel
	}
	return roleUser
}

func joinText(blocks []ir.Block) string {
	var b strings.Builder
	for _, block := range blocks {
		switch block.Type {
		case ir.BlockText:
			b.WriteString(block.Text)
		case ir.BlockToolResult:
			// 嵌套的工具结果内容也要展开，否则内容会静默丢失。
			if block.ToolResult != nil {
				b.WriteString(joinText(block.ToolResult.Content))
			}
		}
	}
	return b.String()
}

func encodeToolConfig(tc *ir.ToolChoice) *wireToolConfig {
	if tc == nil {
		return nil
	}
	cfg := &wireFuncCallCfg{}
	switch tc.Mode {
	case ir.ToolChoiceAuto:
		cfg.Mode = modeAuto
	case ir.ToolChoiceAny:
		cfg.Mode = modeAny
	case ir.ToolChoiceNone:
		cfg.Mode = modeNone
	case ir.ToolChoiceTool:
		// 本协议没有「必须用这一个工具」的模式，用 ANY 加白名单表达。
		cfg.Mode = modeAny
		cfg.AllowedFunctionNames = []string{tc.Name}
	default:
		return nil
	}
	return &wireToolConfig{FunctionCallingConfig: cfg}
}
