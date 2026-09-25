package chatcompletions

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// EncodeRequest 把 IR 编成 /chat/completions 请求体。
//
// 始终写 stream:true 与 stream_options.include_usage：对上游一律流式，
// 客户端要非流式时由数据面聚合；用量必须拿到才能上报调度层。
func EncodeRequest(req *ir.Request) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("chatcompletions: nil request")
	}
	w := wireRequest{
		Model:         req.Model,
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		Stream:        true,
		StreamOptions: &wireStreamOptions{IncludeUsage: true},
	}
	// 混淆开关只在客户端显式表态时写：显式 false 是「关掉上游默认的混淆
	// 保护」，与没提不是一回事，替客户端造键就是替它表态。
	if req.IncludeObfuscation != nil {
		w.StreamOptions.IncludeObfuscation = req.IncludeObfuscation
	}
	if req.MaxTokens > 0 {
		n := req.MaxTokens
		w.MaxTokens = &n
	}
	if len(req.StopSequences) > 0 {
		raw, err := json.Marshal(req.StopSequences)
		if err != nil {
			return nil, fmt.Errorf("chatcompletions: stop: %w", err)
		}
		w.Stop = raw
	}

	if len(req.System) > 0 {
		content, err := encodeContent(req.System)
		if err != nil {
			return nil, fmt.Errorf("chatcompletions: system: %w", err)
		}
		w.Messages = append(w.Messages, wireMessage{Role: roleSystem, Content: content})
	}

	for i, m := range req.Messages {
		msgs, err := encodeMessage(m)
		if err != nil {
			return nil, fmt.Errorf("chatcompletions: messages[%d]: %w", i, err)
		}
		w.Messages = append(w.Messages, msgs...)
	}

	for _, t := range req.Tools {
		def := wireFunctionDef{Name: t.Name, Description: t.Description, Strict: t.Strict}
		if t.Schema != "" {
			def.Parameters = json.RawMessage(t.Schema)
		}
		w.Tools = append(w.Tools, wireTool{Type: "function", Function: def})
	}
	choice, err := encodeToolChoice(req.ToolChoice)
	if err != nil {
		return nil, fmt.Errorf("chatcompletions: tool_choice: %w", err)
	}
	w.ToolChoice = choice

	w.PresencePenalty = req.PresencePenalty
	w.FrequencyPenalty = req.FrequencyPenalty
	w.Seed = req.Seed
	w.N = req.Candidates
	w.LogProbs = req.LogProbs
	w.TopLogProbs = req.TopLogProbs
	w.LogitBias = req.LogitBias
	// anthropic 方言 standard_only 翻译成 default；ultrafast 是 responses
	// 专属，本族值集 provably 装不下——丢弃由 DescribeLossy 报出。
	if tier, ok := codec.MapServiceTier(req.ServiceTier, Name); ok {
		w.ServiceTier = tier
	}
	w.PromptCacheKey = req.PromptCacheKey
	w.ParallelToolCalls = req.ParallelToolCalls
	w.Verbosity = req.Verbosity
	w.SafetyIdentifier = req.SafetyIdentifier
	w.Moderation = req.Moderation
	w.PromptCacheOptions = req.PromptCacheOptions
	w.ResponseFormat = encodeResponseFormat(req.ResponseFormat)

	// chat 一族专属四维原样回写（外族编码器不读它们，跨族损耗由
	// DescribeLossy 报出）。voice 恒写 string 简形：{id} 对象与 string
	// 语义等价，取最简；没给 voice 不造空串——那会被上游当非法音色名。
	//
	// modalities 的官方值集只有 text/audio：客户端递来别的值（如 image）
	// 写出去是上游必 400 的形状，滤掉并由 DescribeLossy 报出。
	for _, m := range req.Modalities {
		if m == "text" || m == "audio" {
			w.Modalities = append(w.Modalities, m)
		}
	}
	if req.AudioOut != nil {
		ao := &wireAudioOut{Format: req.AudioOut.Format}
		if req.AudioOut.Voice != "" {
			v, err := json.Marshal(req.AudioOut.Voice)
			if err != nil {
				return nil, fmt.Errorf("chatcompletions: audio.voice: %w", err)
			}
			ao.Voice = v
		}
		w.Audio = ao
	}
	w.Prediction = req.Prediction
	w.WebSearchOptions = req.WebSearchOptions

	switch {
	case req.Thinking.On():
		w.ReasoningEffort = req.Thinking.Effort
		// 只有 token 预算没有档位时（来自 Anthropic 客户端）折成档位：
		// 本协议无预算概念，不折就等于把思考请求整个丢掉。
		if w.ReasoningEffort == "" {
			w.ReasoningEffort = effortForBudget(req.Thinking.BudgetTokens)
		}
	case req.Thinking.Off():
		// 明确关闭要写出来。不写等于「没提」，上游按自己的默认开启推理，
		// 而客户端刚刚明确说了不要。
		w.ReasoningEffort = effortNone
	}
	// metadata 同族回吐：客户端的关联数据通道，随响应回显。user_id 同时
	// 落 user 字段（顶层字段是滥用追踪的官方槽位）。
	w.Metadata = req.ClientMetadata
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

// encodeMessage 把一条 IR 消息展开成一到多条 wire 消息。
//
// 是一对多而非一对一：IR 的一条 user 消息可能同时含工具结果与文本，
// 本协议要求工具结果各自成一条 tool 消息。
func encodeMessage(m ir.Message) ([]wireMessage, error) {
	var (
		out      []wireMessage
		plain    []ir.Block
		toolMsgs []wireMessage
		calls    []wireToolCall
		thinking strings.Builder
		// cites 与 citeText 服务于消息级 annotations：本协议的偏移量相对
		// 整条消息 content 的拼接文本，而 IR 的引用是块内坐标，必须逐块平移。
		// 拼接顺序与 encodeContent 的全文本收敛一致（按块序串接）。
		cites    []ir.Citation
		citeText strings.Builder
		// refusal 收集历史里的拒绝正文：本族有专属槽位（message.refusal），
		// 落进 plain 会被编成普通文本，客户端就无法区分拒绝与正常回答。
		refusal strings.Builder
	)
	for _, b := range m.Content {
		switch b.Type {
		case ir.BlockToolResult:
			if b.ToolResult == nil {
				return nil, fmt.Errorf("tool_result block without payload")
			}
			// 失败态在这里改写成前缀块：本协议的 tool 消息只有
			// role/tool_call_id/content 三个键，没有放标记的位置，
			// 而丢掉它会让模型把失败当成功。
			//
			// 前置一个文本块而不是把整段内容折成一个字符串：后者会把
			// 工具结果里的媒体块碾平，而那与失败态无关。
			content, err := encodeContent(
				codec.PrefixToolResultError(b.ToolResult, outboundCodec{}.Caps()))
			if err != nil {
				return nil, fmt.Errorf("tool_result content: %w", err)
			}
			toolMsgs = append(toolMsgs, wireMessage{
				Role:       roleTool,
				ToolCallID: b.ToolResult.ToolUseID,
				Content:    content,
			})
		case ir.BlockToolUse:
			if b.ToolUse == nil {
				return nil, fmt.Errorf("tool_use block without payload")
			}
			// arguments 是 JSON 字符串槽位：原文照转义嵌入，请求体不会因此
			// 非法。残缺参数不清空——{} 会让工具不带参数执行，是一次真实
			// 副作用；原文透传让工具侧的解析失败暴露出来，损耗由
			// DescribeLossy 报出。ObjectInput：custom 形态给 {"input":…} 投影。
			args := b.ToolUse.ObjectInput()
			calls = append(calls, wireToolCall{
				ID:       b.ToolUse.ID,
				Type:     "function",
				Function: wireFunctionCall{Name: b.ToolUse.Name, Arguments: args},
			})
		case ir.BlockThinking:
			if b.Thinking != nil {
				thinking.WriteString(b.Thinking.Text)
			}
		case ir.BlockServerToolUse, ir.BlockWebSearchToolResult:
			// 服务端托管工具块没有本族槽位：整块跳过。落进 plain 会让
			// encodeContent 直接报错，伪装成 tool_calls 则是伪造一场
			// 客户端从未发起、也永远等不到结果的调用。
			continue
		case ir.BlockContainerUpload:
			// 容器文件引用块没有本族槽位：整块跳过。落进 plain 会编成一个空
			// content part，伪装成附件则会把只有 file_id 的引用当内联内容投递、
			// 被上游按内容解码后 400。损耗由 DescribeLossy 统一报出。
			continue
		case ir.BlockText:
			// 先在块内解析再平移：块内定位精确，拼接文本里搜可能命中别块。
			cites = append(cites, shiftCitations(b.Citations, citeText.String(), b.Text)...)
			citeText.WriteString(b.Text)
			plain = append(plain, b)
		case ir.BlockRefusal:
			// 历史里的拒绝也要带回：上一轮模型拒绝过是下一轮的上下文，
			// 丢了会让模型看不到自己拒绝过，可能被同样的追问绕过。
			refusal.WriteString(b.Text)
		default:
			plain = append(plain, b)
		}
	}

	// tool 消息必须排在承载它们的助手消息之后、后续正文之前，
	// 而 IR 把工具结果放在 user 消息里，所以先发它们。
	out = append(out, toolMsgs...)

	// 音频引用与拒绝正文都单独算内容：只带 {audio:{id}} 或只带 refusal
	// 而无正文的 assistant 历史消息是官方合法形态，按空消息跳过等于撕掉
	// 上一轮的音频凭证 / 抹掉模型拒绝过的记录。
	if len(plain) == 0 && len(calls) == 0 && thinking.Len() == 0 &&
		m.AudioID == "" && refusal.Len() == 0 {
		return out, nil
	}
	msg := wireMessage{Role: string(m.Role), ToolCalls: calls, ReasoningContent: thinking.String()}
	// annotations 是助手消息专属槽位：历史里的用户消息即使带了引用
	// （跨协议转换的罕见形态）也不写，写出去是非法的消息形状。
	if m.Role == ir.RoleAssistant {
		msg.Annotations = encodeAnnotations(citeText.String(), cites)
		msg.Refusal = refusal.String()
		if m.AudioID != "" {
			// 请求侧只回 {id} 引用形态：完整音频数据不重复回传。
			ref, err := json.Marshal(audioRef{ID: m.AudioID})
			if err != nil {
				return nil, err
			}
			msg.Audio = ref
		}
	}
	if len(plain) > 0 {
		content, err := encodeContent(plain)
		if err != nil {
			return nil, err
		}
		if string(content) == "[]" {
			// 部件被编码器全丢（空壳图片），整条消息落空：上游看来等于
			// 「这一方什么都没说」，客户端也无从分辨是占位还是正文。
			// 落约定占位，与 anthropic / responses 同口径。
			content, err = json.Marshal(codec.ConversationPlaceholder)
			if err != nil {
				return nil, err
			}
		}
		msg.Content = content
	}
	return append(out, msg), nil
}

// encodeContent 全文本时收敛成字符串，含图片时用 parts 数组。
// 字符串形态是本协议的常见写法，兼容性最好。
func encodeContent(blocks []ir.Block) (json.RawMessage, error) {
	onlyText := true
	for _, b := range blocks {
		if b.Type != ir.BlockText {
			onlyText = false
			break
		}
	}
	if onlyText {
		var text strings.Builder
		for _, b := range blocks {
			text.WriteString(b.Text)
		}
		return json.Marshal(text.String())
	}

	parts := make([]wirePart, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case ir.BlockText:
			parts = append(parts, wirePart{Type: partText, Text: b.Text})
		case ir.BlockImage, ir.BlockAudio, ir.BlockDocument, ir.BlockFile:
			if b.Media == nil {
				return nil, fmt.Errorf("%s block without payload", b.Type)
			}
			if b.Type == ir.BlockImage && !b.Media.HasPayload() {
				// 空壳图片整块跳过：照编会写出 {"url":""} 或 "data:;base64,"
				// 的非法形状，上游按 URL 形态校验直接 400，而报错只指向
				// 「图片无效」，读者看不出是哪一段输入害的。只带 file_id 的
				// Responses 图片也落在这里——本族的图片槽位不认 file_id。
				// 损耗由有损诊断报出。
				continue
			}
			part, ok := encodeMediaPart(b)
			if !ok {
				part = wirePart{Type: partText, Text: codec.DowngradeMedia(b).Text}
			}
			parts = append(parts, part)
		default:
			return nil, fmt.Errorf("cannot encode block type %q as content", b.Type)
		}
	}
	return json.Marshal(parts)
}

// encodeMediaPart 把媒体块编成本协议的原生 part。
// 返回 ok=false 表示本协议表达不了，交由调用方降级为文本。
func encodeMediaPart(b ir.Block) (wirePart, bool) {
	if b.Media.FileID != "" && !b.Media.HasPayload() {
		// file 槽位原生收 file_id：同族往返原样带回，不代取内容。
		// 这一支不看媒体类型——引用形态本来就不带字节，类型无从嗅起。
		return wirePart{Type: partFile, File: &wireFile{
			Filename: b.Media.Name,
			FileID:   b.Media.FileID,
		}}, true
	}
	media := codec.SniffMediaType(b.Media)
	// 白名单判定与有损诊断共用 Caps，避免两处漂移。
	if !(outboundCodec{}.Caps().AcceptsMedia(media)) {
		return wirePart{}, false
	}
	switch {
	case strings.HasPrefix(media, "image/"):
		return wirePart{Type: partImageURL, ImageURL: &wireImageURL{
			URL: renderImageURL(b.Media), Detail: b.Media.Detail,
		}}, true

	case strings.HasPrefix(media, "audio/"):
		// input_audio 只接受内联 base64 与它认得的格式名，
		// 远程链接与冷门格式都表达不了。
		format := audioFormat(media)
		if b.Media.Data == "" || format == "" {
			return wirePart{}, false
		}
		return wirePart{Type: partInputAudio, InputAudio: &wireInputAudio{
			Data: b.Media.Data, Format: format,
		}}, true

	case b.Media.Data != "" && media != "":
		return wirePart{Type: partFile, File: &wireFile{
			Filename: b.Media.Name,
			FileData: "data:" + media + ";base64," + b.Media.Data,
		}}, true

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

// renderImageURL 把 IR 的分离字段拼回本协议的单一 url 字段。
func renderImageURL(img *ir.Media) string {
	if img.URL != "" {
		return img.URL
	}
	// 类型嗅不出时不编造一个：谎报的类型会让上游拒收整个请求，
	// 而不带类型的 data URI 多数上游会自行探测。
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
		obj := wireToolChoiceObject{Type: "function"}
		obj.Function.Name = tc.Name
		return json.Marshal(obj)
	default:
		return nil, nil
	}
}

// encodeResponseFormat 写出本协议的 response_format。
//
// schema 形态缺 schema 原文时降级成 json_object 而不是丢掉整个要求：
// 客户端要的最低限度是「输出是 JSON」，这一点仍然能满足。
func encodeResponseFormat(rf *ir.ResponseFormat) *wireResponseFormat {
	if rf == nil {
		return nil
	}
	if rf.Kind == ir.ResponseFormatSchema && rf.Schema != "" {
		return &wireResponseFormat{
			Type: "json_schema",
			JSONSchema: &wireJSONSchema{
				Name:        rf.Name,
				Description: rf.Description,
				Schema:      json.RawMessage(rf.Schema),
				Strict:      rf.Strict,
			},
		}
	}
	return &wireResponseFormat{Type: "json_object"}
}
