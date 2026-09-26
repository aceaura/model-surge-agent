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
		// 角色白名单：anthropic 的 message 只有 user/assistant（system 是顶层
		// 独立字段，已在上面单独解）。此前任意字符串都被 ir.Role(m.Role) 原样
		// 收进 IR：转到 gemini 时 roleFor 把非 assistant 一律映射成 user，语义被
		// 悄悄改写却既不报错也无注记。与 chat/responses 解码器同口径，畸形角色
		// 直接 400，而不是静默兜底成某个默认角色。
		role := ir.Role(m.Role)
		if role != ir.RoleUser && role != ir.RoleAssistant {
			return nil, ir.NewError(ir.ErrInvalidRequest, 400, "invalid_request_error",
				fmt.Sprintf("messages[%d]: unknown role %q", i, m.Role))
		}
		content, err := decodeContent(m.Content)
		if err != nil {
			return nil, wrapField(fmt.Sprintf("messages[%d].content", i), err)
		}
		out.Messages = append(out.Messages, ir.Message{
			Role:    role,
			Content: content,
		})
	}

	// 服务端工具定义再留一份原文：computer 的 display_*、web_fetch 的
	// max_content_tokens 等未建模声明参数，同族回写时靠 ServerRaw 整块保真。
	// 主结构已校验通过，这一遍只按位取原文，解析失败保持 nil 即可。
	var rawTools struct {
		Tools []json.RawMessage `json:"tools"`
	}
	_ = json.Unmarshal(body, &rawTools)
	for i, t := range w.Tools {
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
		if t.CacheControl != nil {
			tool.CacheCtl = t.CacheControl.Type
			tool.CacheTTL = t.CacheControl.TTL
		}
		// custom 是函数工具的显式写法，与省略同义，不当服务端工具记。
		if t.Type != "" && t.Type != "custom" {
			tool.ServerType = t.Type
			// 原文只随服务端工具走：函数工具的同族往返由逐字段建模全量
			// 覆盖，吃原文通道反而会把客户端没给的键凭空带上。
			if i < len(rawTools.Tools) {
				tool.ServerRaw = rawTools.Tools[i]
			}
			// 声明参数同时收成结构化视图：原文是不透明字节，观测面与
			// 内部构造路径要的是可寻址的字段。
			tool.ServerParams = serverParamsOf(t)
		}
		out.Tools = append(out.Tools, tool)
	}
	toolChoice, err := decodeToolChoice(w.ToolChoice)
	if err != nil {
		return nil, wrapField("tool_choice", err)
	}
	out.ToolChoice = toolChoice

	if w.Thinking != nil {
		// type 有 enabled / disabled / adaptive 三种取值，都是客户端的明确
		// 表态，所以这里一定给出 true 或 false，绝不留 nil——nil 是「没提」
		// 那一档。adaptive 是官方推荐的现代形态（enabled 已标废弃）：模型
		// 自主决定思考量，不带预算。
		//
		// 三种之外的取值（大小写写错、上游新增档、畸形）一律 400，不能默认
		// 落进 disabled：那会把「客户端要求思考」静默改写成「明令模型别思考」，
		// 出站编码器还会忠实地把 disabled 写回去。未知枚举拒收与本文件
		// decodeToolChoice、以及 response_format/text.format 的处置一致。
		switch w.Thinking.Type {
		case "enabled", "disabled", "adaptive":
		default:
			return nil, wrapField("thinking.type",
				fmt.Errorf("unknown thinking type %q, want one of enabled/disabled/adaptive", w.Thinking.Type))
		}
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
	// output_format 是同一诉求的 beta 旧槽位（官方已废弃，老客户端还在发）：
	// 同形同判据。两槽同给时 output_config.format 已先落 IR，新槽胜出；
	// 只有新槽缺席才回落旧槽。编码恒写新槽，不产出旧键。
	if out.ResponseFormat == nil {
		if f := w.OutputFormat; f != nil &&
			f.Type == "json_schema" && len(f.Schema) > 0 &&
			string(f.Schema) != "null" {
			out.ResponseFormat = &ir.ResponseFormat{
				Kind:   ir.ResponseFormatSchema,
				Schema: string(f.Schema),
			}
			strict := true
			out.ResponseFormat.Strict = &strict
		}
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
	// 原值进 IR，跨族映射是出站编码的事（codec.MapServiceTier）。
	out.ServiceTier = w.ServiceTier
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
	return decodeBlocks(raw)
}

// decodeBlocks 逐块解析 block 数组。必须逐块而不是一次性 []wireBlock：
// Anthropic 在不同块型上复用同一个键名承载不同形状——source 在 document 块上
// 是对象、在 search_result 块上是字符串。一次性解析时任一块的形状冲突都会让
// 整个 Unmarshal 失败，同消息的其他块（包括用户真正在问的那句话）随之全部蒸发，
// 调用方只拿到一个错误。逐块解析让冲突块降级成不透明块，兄弟块照常解码。
func decodeBlocks(raw json.RawMessage) ([]ir.Block, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(raw, &raws); err != nil {
		return nil, fmt.Errorf("must be a string or a block array: %w", err)
	}
	out := make([]ir.Block, 0, len(raws))
	for _, r := range raws {
		block, ok, err := decodeRawBlock(r)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, block)
		}
	}
	return out, nil
}

// decodeRawBlock 解析单个块的原文。块内字段形状冲突（如 source 在该块型上是
// 字符串而非对象）时整块留成不透明块供同族回吐——丢弃它会破坏 assistant 历史里
// server_tool_use 与结果块的配平，上游按配平校验拒整轮。连判别值都读不出来才算
// 真畸形，报错（与本仓「输入未知即拒」一致）。
func decodeRawBlock(raw json.RawMessage) (ir.Block, bool, error) {
	var b wireBlock
	if err := json.Unmarshal(raw, &b); err != nil {
		if wt := wireTypeOf(raw); wt != "" {
			return ir.Block{Type: ir.BlockOpaque,
				Opaque: &ir.Opaque{WireType: wt, Body: raw, From: Name}}, true, nil
		}
		return ir.Block{}, false, fmt.Errorf("content element is not a valid block: %w", err)
	}
	return decodeBlock(b, raw)
}

// wireTypeOf 只从块原文里抠出判别值 type，供形状冲突时给不透明块定型。
func wireTypeOf(raw json.RawMessage) string {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return ""
	}
	return head.Type
}

// decodeBlock 已知块型的解码。raw 是块的原始 JSON，供 default 分支把未知块
// 整块留成不透明块——未知块型的载荷形状由上游定义，逐字段猜必丢内容。
// 返回 ok=false 表示该块要丢弃（当前仅伴随 error 出现）。
func decodeBlock(b wireBlock, raw json.RawMessage) (ir.Block, bool, error) {
	out := ir.Block{}
	if b.CacheControl != nil {
		out.CacheCtl = b.CacheControl.Type
		out.CacheTTL = b.CacheControl.TTL
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
		// document 的 source.type=content（官方 union 第四种形态，正文是字符串或
		// text/image 块数组）归不透明块，不投进 Media：Media 只有 base64 / URL
		// 两种载体，装不下块数组。此前它落进下面被当 base64 处理——Data 取空、
		// MIME 兜底成 application/pdf，重新编码后写出一个连 data 键都没有的
		// base64 PDF source（官方 Base64PDFSourceParam.data 是 Required），正文
		// 全丢、形状还非法，上游直接 400。归不透明块后同族原样带回无损，跨族由
		// 编码器报错（见 encodeBlock 的 BlockOpaque），两者都比伪造诚实。
		if b.Type == blockDocument && b.Source.Type == "content" {
			out.Type = ir.BlockOpaque
			out.Opaque = &ir.Opaque{WireType: b.Type, Body: raw, From: Name}
			return out, true, nil
		}
		media := &ir.Media{
			MediaType: b.Source.MediaType,
			Data:      b.Source.Data,
			URL:       b.Source.URL,
		}
		if b.Type == blockDocument {
			// document 块的两项配置（用途旁注 context 与引用开关 citations.enabled）
			// 落进 Media：同族逐字往返，跨族由有损诊断报出（见 DocumentConfigDropNote）。
			// citations 键在 document 块上承载 {"enabled":bool} 配置对象而非引用数组，
			// 故走 decodeCitationsConfig 而非 decodeCitations。
			media.Context = b.Context
			media.CitationsEnabled = decodeCitationsConfig(b.Citations)
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
		// 载荷是加密的，本服务无法解读，但同族往返必须逐字保留：Anthropic 的
		// 续话校验要求上一轮的涂抹块原样带回，丢掉它多轮对话会断链。带密文进 IR，
		// 出站时同族（anthropic）逐字回吐，跨族丢弃并报有损。
		out.Type = ir.BlockThinking
		out.Thinking = &ir.Thinking{Redacted: true, RedactedData: b.Data, SignatureFrom: Name}
	case blockServerToolUse:
		// 托管工具调用：放行而不是报 unknown——多轮历史里带 web_search
		// 痕迹的同族往返是合法输入，拒收会让客户端整轮 400。
		out.Type = ir.BlockServerToolUse
		out.ServerToolUse = &ir.ServerToolUse{ID: b.ID, Name: b.Name, Input: string(b.Input)}
	case blockWebSearchToolResult:
		out.Type = ir.BlockWebSearchToolResult
		out.WebSearchToolResult = decodeWebSearchToolResult(b.ToolUseID, b.Content)
	case blockContainerUpload:
		// 容器文件引用：放行而不是报 unknown——多轮历史里带模型产出文件引用的
		// 同族往返是合法输入，拒收会让客户端整轮 400。只有 file_id 一个载荷。
		out.Type = ir.BlockContainerUpload
		out.ContainerUpload = &ir.ContainerUploadRef{FileID: b.FileID}
	default:
		// 未知块型原样留成不透明块，不降级成文本也不报错：Anthropic 的服务端
		// 工具结果块（web_fetch / code_execution / bash_code_execution /
		// text_editor_code_execution / tool_search 等）根本没有 text 字段，降级
		// 等于把抓取的网页正文、stdout、文件内容换成一个空文本块，而兄弟
		// server_tool_use 块还留在原地——发给上游的 tool_use/tool_result 配平当场
		// 断裂。同族逐字回吐无损，跨族由编码器报错（见 encodeBlock 的 BlockOpaque）。
		if b.Type == "" {
			return out, false, fmt.Errorf("block missing type")
		}
		out.Type = ir.BlockOpaque
		out.Opaque = &ir.Opaque{WireType: b.Type, Body: raw, From: Name}
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

func decodeToolChoice(tc *wireToolChoice) (*ir.ToolChoice, error) {
	if tc == nil {
		return nil, nil
	}
	switch tc.Type {
	case "auto":
		return &ir.ToolChoice{Mode: ir.ToolChoiceAuto}, nil
	case "any":
		return &ir.ToolChoice{Mode: ir.ToolChoiceAny}, nil
	case "none":
		return &ir.ToolChoice{Mode: ir.ToolChoiceNone}, nil
	case "tool":
		return &ir.ToolChoice{Mode: ir.ToolChoiceTool, Name: tc.Name}, nil
	default:
		// 未知模式必须报错而非静默回落 nil：nil 是「客户端没提工具选择」那一档，
		// 会把「强制某个工具」悄悄降级成 auto，客户端以为钉死了工具实则没有。
		// 与 chat_completions / responses 两族对未知 mode 一律 400 同口径。
		return nil, fmt.Errorf("unknown tool_choice type %q", tc.Type)
	}
}

// serverParamsOf 把服务端工具的声明参数收进 IR；一个都没给时保持 nil，
// 同族回写一个键也不造（缺省保持缺省）。search_context_size 是 responses
// 原生维度，本协议线体上没有，不进这里。
func serverParamsOf(t wireTool) *ir.ServerParams {
	if t.MaxUses == 0 && len(t.AllowedDomains) == 0 && len(t.BlockedDomains) == 0 && len(t.UserLocation) == 0 {
		return nil
	}
	return &ir.ServerParams{
		MaxUses:        t.MaxUses,
		AllowedDomains: t.AllowedDomains,
		BlockedDomains: t.BlockedDomains,
		UserLocation:   t.UserLocation,
	}
}

func wrapField(field string, err error) error {
	return ir.NewError(ir.ErrInvalidRequest, 400, "invalid_request_error",
		fmt.Sprintf("%s: %v", field, err))
}
