package anthropic

import (
	"encoding/json"
	"fmt"

	"github.com/aceaura/model-surge-agent/backend/codec"
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
		tool := ir.Tool{
			Name:        t.Name,
			Description: t.Description,
			Schema:      string(t.InputSchema),
			Strict:      t.Strict,
			// 2026 修饰四维原样进 IR（服务端工具上也照收——出站按目标能力取舍）。
			DeferLoading:        t.DeferLoading,
			EagerInputStreaming: t.EagerInputStreaming,
			InputExamples:       t.InputExamples,
			AllowedCallers:      t.AllowedCallers,
		}
		// custom 是函数工具的显式写法，与省略同义，不当服务端工具记。
		if t.Type != "" && t.Type != "custom" {
			tool.ServerType = t.Type
		}
		out.Tools = append(out.Tools, tool)
	}
	out.ToolChoice = decodeToolChoice(w.ToolChoice)

	if w.Thinking != nil {
		// type 有 enabled / disabled / adaptive 三种取值，都是客户端的明确
		// 表态，所以这里一定给出 true 或 false，绝不留 nil——nil 是「没提」
		// 那一档。adaptive 是官方推荐的现代形态（enabled 已标废弃）：模型
		// 自主决定思考量，不带预算。
		enabled := w.Thinking.Type == "enabled" || w.Thinking.Type == "adaptive"
		out.Thinking = &ir.ThinkingConfig{
			Enabled:      &enabled,
			Adaptive:     w.Thinking.Type == "adaptive",
			Display:      w.Thinking.Display,
			BudgetTokens: w.Thinking.BudgetTokens,
		}
	}
	if w.Metadata != nil && w.Metadata.UserID != "" {
		out.Metadata = map[string]string{"user_id": w.Metadata.UserID}
	}
	// output_config.format 只有 json_schema 一种 type，且恒为严格语义
	//（没有 strict 开关也没有名称位，与 gemini 的 responseSchema 同款）。
	// 非 json_schema 的 type 与空 schema 都按没给处理：空约束写出来上游也是
	// 自由文本，不能凭空发明一个不存在的诉求进 IR。
	if f := w.OutputConfig; f != nil && f.Format != nil &&
		f.Format.Type == "json_schema" && len(f.Format.Schema) > 0 &&
		string(f.Format.Schema) != "null" {
		out.ResponseFormat = &ir.ResponseFormat{
			Kind:   ir.ResponseFormatSchema,
			Schema: string(f.Format.Schema),
		}
		strict := true
		out.ResponseFormat.Strict = &strict
	}
	// output_config.effort 原值进 IR Thinking.Effort（值集是 OpenAI 的子集，
	// 无需翻译）。effort 独立出现（没带 thinking 块）也算开了思考。
	if f := w.OutputConfig; f != nil && f.Effort != "" {
		if out.Thinking == nil {
			out.Thinking = &ir.ThinkingConfig{Enabled: ir.ThinkingOn()}
		}
		out.Thinking.Effort = f.Effort
	}
	// 顶层缓存便捷糖与推理地理偏好原值进 IR；不展开、不映射。
	if w.CacheControl != nil {
		out.TopCacheCtl = w.CacheControl.Type
		out.TopCacheTTL = w.CacheControl.TTL
	}
	out.InferenceGeo = w.InferenceGeo
	// container 两形态（string 简写 / {id,skills} 对象）统一进 IR。
	ct, err := decodeContainerParam(w.Container)
	if err != nil {
		return nil, wrapField("container", err)
	}
	out.Container = ct
	return out, nil
}

// decodeContainerParam 解请求侧 container：string 简写（仅 id）或
// {id, skills} 对象。空/显式 null 都视为没给。
func decodeContainerParam(raw json.RawMessage) (*ir.Container, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var id string
	if err := json.Unmarshal(raw, &id); err == nil {
		return &ir.Container{ID: id}, nil
	}
	var p containerParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("decode container: %w", err)
	}
	ct := &ir.Container{ID: p.ID}
	for _, s := range p.Skills {
		ct.Skills = append(ct.Skills, ir.Skill{SkillID: s.SkillID, Type: s.Type, Version: s.Version})
	}
	return ct, nil
}

// decodeContainer 响应侧容器回显进 IR。
func decodeContainer(c *container) *ir.Container {
	if c == nil {
		return nil
	}
	ct := &ir.Container{ID: c.ID, ExpiresAt: c.ExpiresAt}
	for _, s := range c.Skills {
		ct.Skills = append(ct.Skills, ir.Skill{SkillID: s.SkillID, Type: s.Type, Version: s.Version})
	}
	return ct
}

// encodeContainerInfo 响应侧回写：IR -> {id, expires_at, skills}。
func encodeContainerInfo(ct *ir.Container) *container {
	if ct == nil {
		return nil
	}
	out := &container{ID: ct.ID, ExpiresAt: ct.ExpiresAt}
	for _, s := range ct.Skills {
		out.Skills = append(out.Skills, containerSkill{SkillID: s.SkillID, Type: s.Type, Version: s.Version})
	}
	return out
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
		out.Citations = decodeCitations(b.Citations)
	case blockImage, blockDocument:
		if b.Source == nil {
			return out, false, fmt.Errorf("%s block needs a source", b.Type)
		}
		media := &ir.Media{
			MediaType: b.Source.MediaType,
			Data:      b.Source.Data,
			URL:       b.Source.URL,
		}
		// 两个容器同形，块类型按 media type 判定而非容器名：
		// document 容器里也可能装别的类型。
		out.Type = codec.MediaKindFor(codec.SniffMediaType(media))
		out.Media = media
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
		// 载荷是加密的，本服务无法解读也无法转给任何上游。这里不丢，
		// 带标记进 IR，由出站编码丢弃并报一条有损说明——
		// 解码期丢掉就再没有痕迹可查了。
		out.Type = ir.BlockThinking
		out.Thinking = &ir.Thinking{Redacted: true, SignatureFrom: Name}
	case blockServerToolUse:
		// 托管工具调用：放行而不是报 unknown——多轮历史里带 web_search
		// 痕迹的同族往返是合法输入，拒收会让客户端整轮 400。
		out.Type = ir.BlockServerToolUse
		out.ServerToolUse = &ir.ServerToolUse{ID: b.ID, Name: b.Name, Input: string(b.Input)}
	case blockWebSearchToolResult:
		out.Type = ir.BlockWebSearchToolResult
		out.WebSearchToolResult = decodeWebSearchToolResult(b.ToolUseID, b.Content)
	default:
		return out, false, fmt.Errorf("unknown block type %q", b.Type)
	}
	return out, true, nil
}

// decodeWebSearchToolResult web_search_tool_result.content -> IR 结果。
// content 是 union：错误形态是单个对象（web_search_tool_result_error），
// 结果形态是子块数组。只按数组解会把错误对象解出零条结果——「搜索失败」
// 被伪造成「搜索成功但没找到东西」，两种语义对客户端完全不同。
func decodeWebSearchToolResult(toolUseID string, raw json.RawMessage) *ir.WebSearchToolResult {
	out := &ir.WebSearchToolResult{ToolUseID: toolUseID}
	if len(raw) == 0 {
		return out
	}
	var eb webSearchToolErrorBlock
	if json.Unmarshal(raw, &eb) == nil && eb.ErrorCode != "" {
		out.ErrorCode = eb.ErrorCode
		return out
	}
	var rs []webSearchResultBlock
	if err := json.Unmarshal(raw, &rs); err != nil {
		return out
	}
	for _, r := range rs {
		out.Results = append(out.Results, ir.WebSearchResult{
			Title: r.Title, URL: r.URL, Snippet: r.EncryptedContent, PageAge: r.PageAge,
		})
	}
	return out
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
