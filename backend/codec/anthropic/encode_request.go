package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// leadingUserPlaceholder 是首条消息不是 user 时补入的占位文本。
const leadingUserPlaceholder = "(continuing the conversation)"

// EncodeRequest 把 IR 编码成 /v1/messages 请求体。
//
// 始终写 stream:true —— 对上游一律流式请求，客户端要非流式时由数据面
// 聚合事件。这样上游只有一条解码路径。
func EncodeRequest(req *ir.Request) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("anthropic: nil request")
	}
	w := wireRequest{
		Model:         req.Model,
		MaxTokens:     req.MaxTokens,
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		TopK:          req.TopK,
		StopSequences: req.StopSequences,
		Stream:        true,
	}
	maxTokens, hasMax, err := codec.MaxTokensFor(req.MaxTokens, outboundCodec{}.Caps())
	if err != nil {
		return nil, fmt.Errorf("anthropic: %w", err)
	}
	if hasMax {
		w.MaxTokens = maxTokens
	}

	if len(req.System) > 0 {
		raw, err := encodeBlocks(req.System)
		if err != nil {
			return nil, fmt.Errorf("anthropic: system: %w", err)
		}
		w.System = raw
	}

	// 本协议要求首条消息是 user，否则整个请求被拒。历史被裁剪成以
	// assistant 起头时（跨协议转换的常见形态）在前面补一条占位消息。
	// 只在确有需要时插入：无条件插入会改变正常请求的前缀，打掉 prompt cache。
	if len(req.Messages) > 0 && req.Messages[0].Role != ir.RoleUser {
		placeholder, err := encodeBlocks([]ir.Block{{Type: ir.BlockText, Text: leadingUserPlaceholder}})
		if err != nil {
			return nil, fmt.Errorf("anthropic: leading user placeholder: %w", err)
		}
		w.Messages = append(w.Messages, wireMessage{Role: string(ir.RoleUser), Content: placeholder})
	}

	for i, m := range req.Messages {
		raw, err := encodeBlocks(m.Content)
		if err != nil {
			return nil, fmt.Errorf("anthropic: messages[%d]: %w", i, err)
		}
		w.Messages = append(w.Messages, wireMessage{Role: string(m.Role), Content: raw})
	}

	for _, t := range req.Tools {
		tool := wireTool{Name: t.Name, Description: t.Description}
		if t.ServerType != "" {
			// 服务端工具的 type 原样写回，不带 input_schema：参数形状由
			// 上游那一版工具自己定义，我方给出的任何 schema 都可能与它冲突。
			tool.Type = t.ServerType
		} else if t.Schema != "" {
			tool.InputSchema = json.RawMessage(t.Schema)
		}
		w.Tools = append(w.Tools, tool)
	}
	w.ToolChoice = encodeToolChoice(req.ToolChoice)

	switch {
	case req.Thinking.Off():
		// 明确关闭要写出来：本协议的 disabled 是显式取值，省略则随模型默认，
		// 而部分模型默认开启推理。
		w.Thinking = &wireThinking{Type: "disabled"}
	case req.Thinking.On():
		th := &wireThinking{Type: "enabled", BudgetTokens: req.Thinking.BudgetTokens}
		// 只有 effort 没有预算时（来自 responses/gemini 客户端）也必须给出预算：
		// Anthropic 的 thinking 无 effort 概念，缺 budget_tokens 会被拒。
		if th.BudgetTokens <= 0 {
			th.BudgetTokens = budgetForEffort(req.Thinking.Effort, w.MaxTokens)
		}
		w.Thinking = th
	}
	if id := req.Metadata["user_id"]; id != "" {
		w.Metadata = &wireMetadata{UserID: id}
	}
	return json.Marshal(w)
}

// budgetForEffort 把 effort 档位折成 token 预算。
// 预算必须小于 max_tokens，否则 Anthropic 会拒绝请求。
func budgetForEffort(effort string, maxTokens int) int {
	ratio := 0.5
	switch effort {
	case "low", "minimal":
		ratio = 0.2
	case "high", "max":
		ratio = 0.8
	}
	budget := int(float64(maxTokens) * ratio)
	// 低于 1024 会被 API 拒绝。
	if budget < 1024 {
		budget = 1024
	}
	if budget >= maxTokens {
		budget = maxTokens - 1
	}
	return budget
}

func encodeBlocks(blocks []ir.Block) (json.RawMessage, error) {
	out := make([]wireBlock, 0, len(blocks))
	for _, b := range blocks {
		wb, ok, err := encodeBlock(b)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, wb)
		}
	}
	return json.Marshal(out)
}

func encodeBlock(b ir.Block) (wireBlock, bool, error) {
	out := wireBlock{}
	if b.CacheCtl != "" {
		out.CacheControl = &wireCacheControl{Type: b.CacheCtl}
	}

	switch b.Type {
	case ir.BlockText:
		out.Type = blockText
		out.Text = b.Text
	case ir.BlockImage, ir.BlockAudio, ir.BlockDocument, ir.BlockFile:
		if b.Media == nil {
			return out, false, fmt.Errorf("%s block without payload", b.Type)
		}
		media := codec.SniffMediaType(b.Media)
		container, ok := anthropicMediaContainer(media)
		if !ok {
			// 本协议只读图片与 PDF；其余附件降级为文本，
			// 让模型知道这里本来有个文件而不是整轮请求被拒。
			return encodeBlock(codec.DowngradeMedia(b))
		}
		out.Type = container
		out.Source = &wireSource{MediaType: media, Data: b.Media.Data, URL: b.Media.URL}
		if b.Media.URL != "" {
			out.Source.Type = "url"
		} else {
			out.Source.Type = "base64"
		}
	case ir.BlockToolUse:
		if b.ToolUse == nil {
			return out, false, fmt.Errorf("tool_use block without payload")
		}
		out.Type = blockToolUse
		out.ID = b.ToolUse.ID
		out.Name = b.ToolUse.Name
		// 入参必须是合法 JSON 对象；流被中断时可能残缺，补成空对象
		// 比发一个语法错误的请求体更好。
		if json.Valid([]byte(b.ToolUse.Input)) {
			out.Input = json.RawMessage(b.ToolUse.Input)
		} else {
			out.Input = json.RawMessage(`{}`)
		}
	case ir.BlockToolResult:
		if b.ToolResult == nil {
			return out, false, fmt.Errorf("tool_result block without payload")
		}
		content, err := encodeBlocks(b.ToolResult.Content)
		if err != nil {
			return out, false, fmt.Errorf("tool_result content: %w", err)
		}
		out.Type = blockToolResult
		out.ToolUseID = b.ToolResult.ToolUseID
		out.Content = content
		out.IsError = b.ToolResult.IsError
	case ir.BlockThinking:
		// Redacted 块的载荷在解码期就已舍弃，编出一个空 thinking 块会被上游拒收。
		if b.Thinking == nil || b.Thinking.Redacted {
			return out, false, nil
		}
		out.Type = blockThinking
		out.Thinking = b.Thinking.Text
		// 签名只在同族协议间有效：别家协议的签名发给 Anthropic 会被拒，
		// 丢掉签名后该块作为纯文本推理仍可被接受。判定与有损诊断共用一处出处。
		if !codec.ForeignSignature(b.Thinking, Name) {
			out.Signature = b.Thinking.Signature
		}
	default:
		return out, false, fmt.Errorf("cannot encode block type %q", b.Type)
	}
	return out, true, nil
}

// anthropicMediaContainer 给出 media type 在本协议里的承载块名。
// 本协议只读图片与 PDF，其余返回 ok=false 交由调用方降级。
func anthropicMediaContainer(media string) (string, bool) {
	// 白名单判定与有损诊断共用 Caps，避免两处漂移。
	if !(outboundCodec{}.Caps().AcceptsMedia(media)) {
		return "", false
	}
	switch {
	case strings.HasPrefix(media, "image/"):
		return blockImage, true
	case media == "application/pdf":
		return blockDocument, true
	default:
		return "", false
	}
}

func encodeToolChoice(tc *ir.ToolChoice) *wireToolChoice {
	if tc == nil {
		return nil
	}
	switch tc.Mode {
	case ir.ToolChoiceAuto:
		return &wireToolChoice{Type: "auto"}
	case ir.ToolChoiceAny:
		return &wireToolChoice{Type: "any"}
	case ir.ToolChoiceNone:
		return &wireToolChoice{Type: "none"}
	case ir.ToolChoiceTool:
		return &wireToolChoice{Type: "tool", Name: tc.Name}
	default:
		return nil
	}
}
