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
			Strict:      t.Strict,
		})
	}
	choice, err := decodeToolChoice(w.ToolChoice)
	if err != nil {
		return nil, badRequest(fmt.Sprintf("tool_choice: %v", err))
	}
	out.ToolChoice = choice

	out.Include = w.Include
	// background 收进 IR 只为可见与可报：本服务是同步流式中继，出站
	// 永不回写（写回与强制的 stream:true + store:false 矛盾），丢弃由
	// DescribeLossy 报出。
	out.Background = w.Background
	out.Truncation = w.Truncation
	out.MaxToolCalls = w.MaxToolCalls
	if w.StreamOptions != nil {
		out.IncludeObfuscation = w.StreamOptions.IncludeObfuscation
	}
	out.ClientMetadata = w.Metadata
	out.ServiceTier = w.ServiceTier
	out.PromptCacheKey = w.PromptCacheKey
	out.SafetyIdentifier = w.SafetyIdentifier
	// 显式 null 等同没给（与 chat 同款归一）。
	if string(w.Moderation) != "null" {
		out.Moderation = w.Moderation
	}
	if string(w.PromptCacheOptions) != "null" {
		out.PromptCacheOptions = w.PromptCacheOptions
	}
	out.ParallelToolCalls = w.ParallelToolCalls
	out.TopLogProbs = w.TopLogProbs
	if w.Text != nil {
		out.Verbosity = w.Text.Verbosity
		out.ResponseFormat = decodeTextFormat(w.Text.Format)
	}

	// reasoning 的四个子参数逐轴收下。此前只在 effort 非空时才建 Thinking，
	// 客户端单给 {"summary":"detailed"} 会让整个对象连 summary 一起消失。
	// Enabled 只由 effort 决定：summary/context/mode 都不是「要不要思考」的
	// 表态，只给子参数时开关保持三态的「没提」。
	// minimal 算开思考——它是「最少的思考」，不是「不思考」；chat 入站同款
	// 值就是这么读的，两族口径必须一致，否则同一个 vendor 值换个入口就变成
	// 相反语义（出站按 Off 写 effort=none，彻底掐掉客户端要的思考）。
	if w.Reasoning != nil {
		th := &ir.ThinkingConfig{Summary: w.Reasoning.Summary}
		switch w.Reasoning.Effort {
		case "":
			// 没表态开关，Effort 留空。
		case effortNone:
			// "none" 是明确关闭，不是强度档位——同 chat_completions。
			th.Enabled = ir.ThinkingOff()
		default:
			th.Enabled = ir.ThinkingOn()
			th.Effort = w.Reasoning.Effort
		}
		// 显式 null 等同没给（Moderation / Prediction 同款归一）：
		// 不归一的话这个 "null" 会被当成客户端给过的值写回线上。
		if string(w.Reasoning.Context) != "null" {
			th.Context = w.Reasoning.Context
		}
		if string(w.Reasoning.Mode) != "null" {
			th.Mode = w.Reasoning.Mode
		}
		out.Thinking = th
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
			appendBlocks(out, ir.RoleAssistant, blocks, item.ID)
		default:
			appendBlocks(out, ir.RoleUser, blocks, item.ID)
		}
		return nil

	case itemFunctionCall:
		appendBlocks(out, ir.RoleAssistant, []ir.Block{{
			Type: ir.BlockToolUse,
			ToolUse: &ir.ToolUse{
				ID:     item.CallID,
				Name:   item.Name,
				Input:  item.Arguments,
				ItemID: item.ID,
			},
		}}, "")
		return nil

	case itemCustomToolCall:
		// 自定义工具的历史调用条目：入参是自由文本。Input 里同时放一份
		// {"input":…} 投影——别族协议只有 JSON 参数槽位，投影让跨族编码
		// 无需知道 Kind 就能降级出合法形状；同族回写走 InputText 原文。
		appendBlocks(out, ir.RoleAssistant, []ir.Block{{
			Type: ir.BlockToolUse,
			ToolUse: &ir.ToolUse{
				ID:        item.CallID,
				Name:      item.Name,
				Kind:      ir.ToolCustom,
				InputText: item.Input,
				Input:     string(ir.MarshalCustomInput(item.Input)),
				ItemID:    item.ID,
			},
		}}, "")
		return nil

	case itemCustomToolCallOutput:
		// output 与 function_call_output 同款双形态（字符串或 part 数组），
		// 复用同一套解析；失败前缀也照认——那是我们出站写的，换目标重试时
		// 不认回来模型会把失败当成功。
		content := decodeToolCallOutput(item.Output)
		content, isErr := codec.AdoptToolResultError(content)
		appendBlocks(out, ir.RoleUser, []ir.Block{{
			Type: ir.BlockToolResult,
			ToolResult: &ir.ToolResult{
				ToolUseID: item.CallID,
				Kind:      ir.ToolCustom,
				Content:   content,
				IsError:   isErr,
			},
		}}, "")
		return nil

	case itemFunctionCallOutput:
		// output 官方允许字符串或 content part 数组两种形态。
		content := decodeToolCallOutput(item.Output)
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
		}}, "")
		return nil

	case itemReasoning:
		text := joinSummary(item.Summary)
		contentChannel := false
		if text == "" {
			// summary 为空时正文可能在 content 数组（reasoning_text part，
			// 官方 ResponseReasoningItem.Content）：不读等于把整条思考正文
			// 静默丢掉，只剩 encrypted_content 签名。走 content 通道的要标记
			// ContentChannel——同族回写时发 reasoning_text 而非 summary_text。
			text = decodeReasoningContent(item.Content)
			contentChannel = text != ""
		}
		if text == "" && item.EncryptedContent == "" {
			return nil
		}
		appendBlocks(out, ir.RoleAssistant, []ir.Block{{
			Type: ir.BlockThinking,
			Thinking: &ir.Thinking{
				Text: text,
				// 加密的推理内容当作签名透传：语义相同（只对同族协议有效，
				// 别家无法解读），复用 SignatureFrom 就不必给 IR 加字段。
				Signature:      item.EncryptedContent,
				SignatureFrom:  Name,
				ItemID:         item.ID,
				ContentChannel: contentChannel,
			},
		}}, "")
		return nil

	default:
		return fmt.Errorf("unknown item type %q", item.Type)
	}
}

// decodeToolCallOutput 解 function_call_output.output 的双形态。
// 字符串形态最常见，落成单个文本块；数组形态（output_text / input_image
// 等 part）走与消息 content 同款的逐 part 解析。缺省、空串或数组解不出
// 都落一个空文本块占位：结果块的内容全丢会让配平的 tool_use 读到
// 不存在的结果，比空结果更难排查。
func decodeToolCallOutput(raw json.RawMessage) []ir.Block {
	if blocks, err := decodeContent(raw); err == nil && len(blocks) > 0 {
		return blocks
	}
	return []ir.Block{{Type: ir.BlockText}}
}

// appendBlocks 把块并进末尾消息，角色不同才新开一条。
// 本协议一个逻辑回合会拆成多个条目，逐条建消息会产出大量单块消息，
// 转成 Anthropic 时因为角色必须交替而被拒。
//
// itemID 只在 message 条目上有值：新建消息时落进 Message.ItemID，同族
// 回写按原号带回（store=true 链上上游按它索引）。块条目（function_call /
// reasoning 等）传空——它们的 id 落在各自的 ToolUse/Thinking.ItemID 上，
// 而非所属消息。合并进已有消息时后到的 message 条目覆盖 ItemID：一个逻辑
// 回合通常只有一个 message 条目，多个时以最后到达者为准。
func appendBlocks(out *ir.Request, role ir.Role, blocks []ir.Block, itemID string) {
	if len(blocks) == 0 {
		return
	}
	if n := len(out.Messages); n > 0 && out.Messages[n-1].Role == role {
		out.Messages[n-1].Content = append(out.Messages[n-1].Content, blocks...)
		if itemID != "" {
			out.Messages[n-1].ItemID = itemID
		}
		return
	}
	out.Messages = append(out.Messages, ir.Message{Role: role, Content: blocks, ItemID: itemID})
}

func joinSummary(items []wireSummary) string {
	var b strings.Builder
	for _, s := range items {
		b.WriteString(s.Text)
	}
	return b.String()
}

// decodeReasoningContent 解 reasoning 条目的 content 数组（reasoning_text
// part，官方 ResponseReasoningItem.Content）。summary 为空时这是思考正文的
// 唯一来源：不读它，只剩 encrypted_content 签名的条目会看不出模型想了什么。
// 解析失败或没有 reasoning_text part 都返回空串，交由调用方按签名有无决定去留。
func decodeReasoningContent(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var parts []wirePart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == partReasoningText {
			b.WriteString(p.Text)
		}
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
			out = append(out, ir.Block{Type: ir.BlockText, Text: p.Text,
				Citations: decodeAnnotations(p.Annotations)})
		case partRefusal:
			// 拒绝正文是可见内容而非元数据，且本族有专属槽位：解成独立的
			// refusal 块，同族往返才能原样回到 refusal part。并进文本块会让
			// 客户端无法区分「模型拒绝了」与「模型这么答的」。
			out = append(out, ir.Block{Type: ir.BlockRefusal, Text: p.Refusal})
		case partInputImage:
			var url, nested string
			if p.ImageURL != nil {
				url, nested = p.ImageURL.URL, p.ImageURL.Detail
			}
			media := decodeImageURL(url)
			media.FileID = p.FileID
			// detail 的规范位置是 part 顶层；chat 形态把它嵌在 image_url
			// 对象里。两处都给了以顶层为准——那是本族自己的键位。
			media.Detail = p.Detail
			if media.Detail == "" {
				media.Detail = nested
			}
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
				// file_id 指向上游已存的文件，本服务不解引用：
				// 原样进 FileID，同族编码时带回；当 URL 透传会让别族
				// 上游拿一个 id 去当链接抓。
				media.FileID = p.FileID
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
		Type  string            `json:"type"`
		Name  string            `json:"name"`
		Mode  string            `json:"mode"`
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("must be a string or a function object: %w", err)
	}
	// allowed_tools 是 responses/codex 一族的白名单形态：type 说的是「这是
	// 一条选择策略」而不是某个已声明工具的类型，内层 mode 才说要不要必须
	// 调。此前对象分支只认 name，这个形态没有 name，于是整个 tool_choice
	// 被 400 拒掉（"name is required"）——客户端既拿不到限制也拿不到注记。
	// 收窄落地见 codec.ShapeRequest 的 enforceToolAllowlist。
	if obj.Type == "allowed_tools" {
		out := &ir.ToolChoice{Mode: ir.ToolChoiceAuto}
		if obj.Mode == "required" {
			out.Mode = ir.ToolChoiceAny
		}
		out.AllowedTools = decodeAllowedToolNames(obj.Tools)
		return out, nil
	}
	// typed 变体（{"type":"mcp"/"file_search"/"computer_use"/...}，官方
	// ToolChoiceTypesParam 与 ToolChoiceMcpParam）：没有 name，字段随变体
	// 互不相同，没有跨族统一维度可建模。整个进 Raw 不透明槽，同族出站
	// 原样回写；外族编不出对应形状（tool_choice 缺省），损耗由
	// DescribeLossy 报出。此前这个形态被下面的 "name is required" 400
	// 拒掉——客户端的合法请求根本进不来。function/custom 是已建模变体，
	// 缺 name 依然是客户端错误，照旧拒（Raw 收下只会在上游再挨一次 400）。
	if obj.Type != "" && obj.Type != "function" && obj.Type != "custom" {
		return &ir.ToolChoice{Raw: append(json.RawMessage(nil), raw...)}, nil
	}
	if obj.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	out := &ir.ToolChoice{Mode: ir.ToolChoiceTool, Name: obj.Name}
	if obj.Type != "" {
		// 带 type 的已建模变体（function / custom）原文进 Raw：同族回写
		// 逐字保留客户端的形状（custom 指名换成 function 会让上游找不到
		// 工具），结构化字段照常供整形与跨族使用。
		out.Raw = append(json.RawMessage(nil), raw...)
	}
	return out, nil
}

// decodeAllowedToolNames 取 allowed_tools.tools 里的工具名。条目通常是
// {"type":"function","name":...}，chat 风格的 {"function":{"name":...}} 与
// 裸字符串形态也照收——白名单本质是一串名字。认不出来就当没有：宁可少收
// 窄（无从收窄时整形阶段会报出）也不要凭空捏一个名字进去。
func decodeAllowedToolNames(list []json.RawMessage) []string {
	var names []string
	for _, item := range list {
		var s string
		if err := json.Unmarshal(item, &s); err == nil {
			if s != "" {
				names = append(names, s)
			}
			continue
		}
		var obj struct {
			Name     string `json:"name"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if err := json.Unmarshal(item, &obj); err != nil {
			continue
		}
		name := obj.Name
		if name == "" {
			name = obj.Function.Name
		}
		if name != "" {
			names = append(names, name)
		}
	}
	return names
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
			Kind:        ir.ResponseFormatSchema,
			Name:        w.Name,
			Description: w.Description,
			Schema:      string(w.Schema),
			Strict:      w.Strict,
		}
	default:
		return nil
	}
}
