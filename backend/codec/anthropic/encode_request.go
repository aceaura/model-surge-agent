package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

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
		placeholder, err := encodeBlocks([]ir.Block{{Type: ir.BlockText, Text: codec.ConversationPlaceholder}})
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
		if string(raw) == "[]" && (len(m.Content) > 0 || m.AudioID != "") {
			// 部件被编码器全丢（空壳图片等），空 content 数组会被上游按校验
			// 拒整轮，比丢内容更糟。落约定占位，与 chat / responses 同口径。
			// AudioID -only 的 assistant 历史（chat 音频引用，本协议无槽位）
			// 同样命中：引用本身被丢弃已由 DescribeLossy 报出，但消息不能
			// 以空 content 形态发给上游。
			raw, err = encodeBlocks([]ir.Block{{Type: ir.BlockText, Text: codec.ConversationPlaceholder}})
			if err != nil {
				return nil, fmt.Errorf("anthropic: messages[%d] placeholder: %w", i, err)
			}
		}
		w.Messages = append(w.Messages, wireMessage{Role: string(m.Role), Content: raw})
	}

	for _, t := range req.Tools {
		if t.ServerType != "" && len(t.ServerRaw) > 0 {
			// 同族来路且有原文的服务端工具：整块回吐——未建模的声明参数
			// （computer 的 display_*、web_fetch 的 max_content_tokens 及
			// 未来新增键）逐字段建模永远慢半拍，重建必丢。ServerRaw 只在
			// anthropic 入站时填充，走到这里必是同族来路；没有 ServerType
			// 的原文不吃（函数工具不得走原文通道）。
			w.Tools = append(w.Tools, wireTool{Raw: t.ServerRaw})
			continue
		}
		tool := wireTool{
			Name:                t.Name,
			Description:         t.Description,
			Strict:              t.Strict,
			DeferLoading:        t.DeferLoading,
			EagerInputStreaming: t.EagerInputStreaming,
			InputExamples:       t.InputExamples,
			AllowedCallers:      t.AllowedCallers,
		}
		if t.CacheCtl != "" {
			tool.CacheControl = &wireCacheControl{Type: t.CacheCtl, TTL: t.CacheTTL}
		}
		if t.ServerType != "" {
			// 服务端工具的 type 原样写回，不带 input_schema：参数形状由
			// 上游那一版工具自己定义，我方给出的任何 schema 都可能与它冲突。
			tool.Type = t.ServerType
			// 声明参数只在服务端工具上写（函数工具线体没有这些键）。有原文
			// 的已在上面整块回吐，这条路径服务的是无原文的内部构造工具；
			// ServerParams 为 nil 时一个键也不造（缺省保持缺省）。
			// SearchContextSize 是 responses 原生维度，本协议无槽位不造键。
			if p := t.ServerParams; p != nil {
				tool.MaxUses = p.MaxUses
				tool.AllowedDomains = p.AllowedDomains
				tool.BlockedDomains = p.BlockedDomains
				tool.UserLocation = p.UserLocation
			}
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
		if req.Thinking.Adaptive {
			// adaptive 是官方推荐的现代形态（enabled 已标废弃）：模型自主
			// 决定思考量，不带预算；display 仅在本族有意义，原样回写。
			w.Thinking = &wireThinking{Type: "adaptive", Display: req.Thinking.Display}
		} else {
			th := &wireThinking{Type: "enabled", BudgetTokens: req.Thinking.BudgetTokens, Display: req.Thinking.Display}
			// 只有 effort 没有预算时（来自 responses/gemini 客户端）也必须给出预算：
			// Anthropic 的 enabled 档无 effort 概念，缺 budget_tokens 会被拒。
			if th.BudgetTokens <= 0 {
				th.BudgetTokens = budgetForEffort(req.Thinking.Effort, w.MaxTokens)
			}
			w.Thinking = th
		}
	}
	// safety_identifier 与 user_id 同一维度（滥用检测标识）：metadata.user_id
	// 槽空着时映进去；两边都有时 user_id 优先，safety_identifier 由诊断报出。
	id := req.Metadata["user_id"]
	if id == "" {
		id = req.SafetyIdentifier
	}
	if id != "" {
		w.Metadata = &wireMetadata{UserID: id}
	}
	// 只回写 schema 约束形态：纯 JSON 模式（没给 schema）在 anthropic 没有
	// 对应物，写出来上游也读不懂——那一档由诊断报出（ResponseSchema 真而
	// ResponseFormat 假）。Name / Strict 也没有槽位，不带过去。
	if rf := req.ResponseFormat; rf != nil && rf.Kind == ir.ResponseFormatSchema && rf.Schema != "" {
		w.OutputConfig = &wireOutputConfig{Format: &wireJSONOutputFormat{
			Type:   "json_schema",
			Schema: json.RawMessage(rf.Schema),
		}}
	}
	// effort 在 anthropic 是封闭五值集（low/medium/high/xhigh/max，没有
	// none/minimal）：装不下的档位丢弃，由诊断报出；"none" 与未开思考同义，
	// 静默即可。复用上面可能已建的 output_config。
	if req.Thinking != nil {
		switch req.Thinking.Effort {
		case "low", "medium", "high", "xhigh", "max":
			if w.OutputConfig == nil {
				w.OutputConfig = &wireOutputConfig{}
			}
			w.OutputConfig.Effort = req.Thinking.Effort
		}
	}
	// 顶层缓存便捷糖与推理地理偏好原样回写：两者都是本协议专属维度，
	// 同族保真，跨族由诊断报出。
	if req.TopCacheCtl != "" {
		w.CacheControl = &wireCacheControl{Type: req.TopCacheCtl, TTL: req.TopCacheTTL}
	}
	w.InferenceGeo = req.InferenceGeo
	// container 回写：仅 id 无技能时用 string 简写形态（官方简写与对象
	// {id} 无 skills 语义等价，取最简）；带技能时用对象形态。
	if req.Container != nil {
		if len(req.Container.Skills) == 0 {
			w.Container, _ = json.Marshal(req.Container.ID)
		} else {
			p := containerParams{ID: req.Container.ID}
			for _, s := range req.Container.Skills {
				p.Skills = append(p.Skills, containerSkill{SkillID: s.SkillID, Type: s.Type, Version: s.Version})
			}
			w.Container, _ = json.Marshal(p)
		}
	}
	// 值集装不下的档位（flex/scale/priority/fast/ultrafast 等）丢弃，
	// 由 DescribeLossy 报出；default 翻译成 standard_only（语义相同）。
	if tier, ok := codec.MapServiceTier(req.ServiceTier, Name); ok {
		w.ServiceTier = tier
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
		out.CacheControl = &wireCacheControl{Type: b.CacheCtl, TTL: b.CacheTTL}
	}

	switch b.Type {
	case ir.BlockText:
		out.Type = blockText
		out.Text = b.Text
		// 带 Raw 的原样带回、投影来的重建（见 citation.go），产物是逐条的
		// 原文数组；wireBlock.Citations 是 RawMessage（document 块会拿同名键
		// 承载配置对象，不能声明成数组），所以这里再包一层 Marshal。
		if cs := encodeCitations(b.Text, b.Citations); len(cs) > 0 {
			raw, err := json.Marshal(cs)
			if err != nil {
				return out, false, err
			}
			out.Citations = raw
		}
	case ir.BlockRefusal:
		// 本协议没有 refusal 槽位（只有 stop_reason=refusal），正文降级为
		// 文本而不是丢弃——拒绝正文是模型真正说出的话，丢了客户端只剩
		// 空消息配一个拒绝标记。不加标注前缀：正文会成为模型后续轮次
		// 读到的自己说过的话，前缀会污染它。
		out.Type = blockText
		out.Text = b.Text
	case ir.BlockImage, ir.BlockAudio, ir.BlockDocument, ir.BlockFile:
		if b.Media == nil {
			return out, false, fmt.Errorf("%s block without payload", b.Type)
		}
		if !b.Media.HasPayload() {
			// 空壳媒体整块跳过（不止图片）：官方 source 只有 base64（media_type
			// 与 data 都是 Required）与 url 两种，两者皆空编出来是缺必填键的形状
			// ——图片是 {"type":"image","source":{"type":"base64"}}，文档是
			// {"type":"document","source":{"type":"base64","media_type":"application/pdf"}}
			// （文件名嗅出的 media_type 在，data 因 omitempty 蒸发）——上游 400 拒
			// 整轮。常见来源是 Responses 客户端只给了 file_id，而本族没有「引用上游
			// 文件服务里的附件」这一维。损耗由有损诊断报出。
			return out, false, nil
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
		if container == blockDocument {
			// document 专属的两个配置键同族回写：context 是给模型的用途说明，
			// citations 是 {"enabled":bool} 配置对象（不是引用数组）。跨族投影来
			// 的附件没有这两维，字段为零值时 omitempty 自然不带出。
			out.Context = b.Media.Context
			if b.Media.CitationsEnabled != nil {
				raw, err := json.Marshal(citationsConfig{Enabled: *b.Media.CitationsEnabled})
				if err != nil {
					return out, false, err
				}
				out.Citations = raw
			}
		}
	case ir.BlockToolUse:
		if b.ToolUse == nil {
			return out, false, fmt.Errorf("tool_use block without payload")
		}
		out.Type = blockToolUse
		out.ID = b.ToolUse.ID
		out.Name = b.ToolUse.Name
		// input 是 RawMessage 对象槽位：残缺/非对象参数直接放进去会炸成
		// 语法错误的请求体，静默换成 {} 则让工具不带参数执行（真实副作用，
		// 比 400 更糟）。规整把原文挪进 ir.RawArgsKey 键位保真。
		// ObjectInput：custom 形态的自由文本以 {"input":…} 投影落进对象槽。
		out.Input, _ = ir.NormalizeToolInput([]byte(b.ToolUse.ObjectInput()))
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
		if b.Thinking == nil {
			return out, false, nil
		}
		if b.Thinking.Redacted {
			// 同族往返：涂抹块逐字回传（redacted_thinking + data 原文）。Anthropic
			// 的续话校验要求上一轮的涂抹块原样带回，丢掉它多轮对话会断链。这个
			// encodeBlock 同时服务出站请求、非流式响应与流式响应三条 anthropic 编码
			// 路径，改一处三处都保真。载荷为空时无从伪造（空 data 会被上游拒收），
			// 仍跳过——由有损诊断报出。
			if b.Thinking.RedactedData == "" {
				return out, false, nil
			}
			out.Type = blockRedactedThinking
			out.Data = b.Thinking.RedactedData
			return out, true, nil
		}
		out.Type = blockThinking
		out.Thinking = b.Thinking.Text
		// 签名只在同族协议间有效：别家协议的签名发给 Anthropic 会被拒，
		// 丢掉签名后该块作为纯文本推理仍可被接受。判定与有损诊断共用一处出处。
		if !codec.ForeignSignature(b.Thinking, Name) {
			out.Signature = b.Thinking.Signature
		}
		// 空壳判据（与 lossy.go 的 CountResponseEmptyThinking 同口径）：
		// 正文为空且没有可写回的签名时，本块 marshal 出来是 {"type":"thinking"}
		// ——thinking 键随 omitempty 蒸发，Anthropic 拒收缺 thinking 字段的块，
		// 整份请求/响应会因一个空块 400。整块跳过，有损诊断报出。
		// 空正文但签名可写回不是空壳：签名本身就是载荷，扩展思考续话的合法形态。
		if out.Thinking == "" && out.Signature == "" {
			return out, false, nil
		}
	case ir.BlockServerToolUse:
		if b.ServerToolUse == nil {
			return out, false, fmt.Errorf("server_tool_use block without payload")
		}
		out.Type = blockServerToolUse
		out.ID = b.ServerToolUse.ID
		out.Name = b.ServerToolUse.Name
		// 与 tool_use 同口径：对象槽位，空与畸形都要规整（空补 {}，
		// 畸形挪进 RawArgsKey），否则整份请求体 marshal 失败。
		out.Input, _ = ir.NormalizeToolInput([]byte(b.ServerToolUse.Input))
	case ir.BlockWebSearchToolResult:
		if b.WebSearchToolResult == nil {
			return out, false, fmt.Errorf("web_search_tool_result block without payload")
		}
		out.Type = blockWebSearchToolResult
		out.ToolUseID = b.WebSearchToolResult.ToolUseID
		// content 是 union：错误形态回错误对象，结果形态回子块数组。
		// 把错误编成空数组就是把「搜索失败」伪造成「成功但没找到」。
		var content any
		if b.WebSearchToolResult.ErrorCode != "" {
			content = webSearchToolErrorBlock{
				Type: "web_search_tool_result_error", ErrorCode: b.WebSearchToolResult.ErrorCode,
			}
		} else {
			rs := make([]webSearchResultBlock, 0, len(b.WebSearchToolResult.Results))
			for _, r := range b.WebSearchToolResult.Results {
				rs = append(rs, webSearchResultBlock{
					Type: "web_search_result", Title: r.Title, URL: r.URL,
					EncryptedContent: r.Snippet, PageAge: r.PageAge,
				})
			}
			content = rs
		}
		raw, err := json.Marshal(content)
		if err != nil {
			return out, false, fmt.Errorf("web_search_tool_result content: %w", err)
		}
		out.Content = raw
	case ir.BlockContainerUpload:
		// 容器文件引用：只有 file_id 一个载荷。ContainerUpload 为 nil 时
		// FileID 留空——上游按缺 file_id 拒收，而不是本服务伪造一个引用。
		out.Type = blockContainerUpload
		if b.ContainerUpload != nil {
			out.FileID = b.ContainerUpload.FileID
		}
	case ir.BlockOpaque:
		// 同族逐字回吐：整块原文经 wireBlock.Raw 原样写出（MarshalJSON 见到 Raw
		// 就整块吐），未建模的键一个不丢——web_fetch_tool_result 的 caller、
		// code_execution_tool_result 的 stdout 都靠这条通道在多轮历史里活下来。
		// 跨族报错而非降级或丢弃：把别家的块型逐字发给目标上游会被按块型校验直接
		// 400，降级成文本会把别家载荷拼进正文污染回答，两者都比响亮拒绝更糟
		// （与本仓「输入未知即拒、不静默伪造」一致，见 ir.BlockOpaque）。
		o := b.Opaque
		if !codec.OpaqueVerbatimFor(o, Name) {
			wt, from := "", ""
			if o != nil {
				wt, from = o.WireType, o.From
			}
			return out, false, fmt.Errorf(
				"cannot carry opaque block %q produced by protocol %q into %s: the block type is only defined in the protocol that produced it", wt, from, Name)
		}
		out.Type = o.WireType
		out.Raw = o.Body
		return out, true, nil
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

// mediaDowngraded 报告一个媒体块是否会被 encodeBlock 降级为文本：有载荷、
// 但嗅出的 media type 不在本协议白名单（图片 + PDF）内。空壳（无载荷）走的是
// 整块跳过路径、不是降级，故不算在内。判据与 encodeBlock 的 !ok 分支同源
// （同样调 anthropicMediaContainer），非流式响应扫描与流式开块共用它，两条
// 路径报同一件事、不会漂移。
func mediaDowngraded(b ir.Block) bool {
	switch b.Type {
	case ir.BlockImage, ir.BlockAudio, ir.BlockDocument, ir.BlockFile:
	default:
		return false
	}
	if b.Media == nil || !b.Media.HasPayload() {
		return false
	}
	_, ok := anthropicMediaContainer(codec.SniffMediaType(b.Media))
	return !ok
}

// countResponseDowngradedMedia 数出非流式响应里会被降级为文本的媒体块：图片
// 单列、音频/文档/文件合列，与 codec.MediaOutputDropNote 的两类计数对应。
// 请求侧的同类降级由 describeBlocksLossy 报出，响应侧此前完全静默——上游
// （如返回音频的 gemini/chat）产出的媒体到了 anthropic 客户端只剩一段
// 「[附件略]」占位文本，客户端无从知道这里本来有个文件。
func countResponseDowngradedMedia(resp *ir.Response) (images, files int) {
	if resp == nil {
		return 0, 0
	}
	for _, b := range resp.Content {
		if !mediaDowngraded(b) {
			continue
		}
		if b.Type == ir.BlockImage {
			images++
		} else {
			files++
		}
	}
	return images, files
}

// mediaEmptyShell 报告一个媒体块是否会被 encodeBlock 整块跳过：是媒体类型、
// Media 非 nil、但三载体（base64 / URL / 文件引用）全空即 !HasPayload()。
// 与 mediaDowngraded 互斥——那条要求 HasPayload()（有载荷但类型不支持，降级
// 为文本），这条要求 !HasPayload()（无载荷，无从编起，跳过）。Media 为 nil
// 不在此列：那是 encodeBlock 直接报错的形状，不是静默跳过。
//
// 判据与 encodeBlock 媒体分支的 `!b.Media.HasPayload() → return out, false, nil`
// 逐字同源，非流式响应扫描与流式开块共用，两条路径报同一件事。
func mediaEmptyShell(b ir.Block) bool {
	switch b.Type {
	case ir.BlockImage, ir.BlockAudio, ir.BlockDocument, ir.BlockFile:
	default:
		return false
	}
	return b.Media != nil && !b.Media.HasPayload()
}

// countResponseEmptyMedia 数出非流式响应里因无任何可投递载荷而被整块跳过的
// 媒体块。请求侧同类空壳由 describeBlocksLossy / describeImageLossy 报「carries
// no payload」，响应侧此前静默——而 encodeBlock 跳过分支的注释写着「损耗由
// 有损诊断报出」，那句在响应路径上并不成立。这里补齐，使注释在两条路径都为真。
func countResponseEmptyMedia(resp *ir.Response) int {
	if resp == nil {
		return 0
	}
	n := 0
	for _, b := range resp.Content {
		if mediaEmptyShell(b) {
			n++
		}
	}
	return n
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
