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

	w.Include = req.Include
	w.Truncation = req.Truncation
	w.Metadata = req.ClientMetadata
	w.ServiceTier = req.ServiceTier
	w.ParallelToolCalls = req.ParallelToolCalls
	w.TopLogProbs = req.TopLogProbs
	// 本协议没有独立的 logprobs 开关，top_logprobs 兼任开关与档位。
	// 客户端只给了开关时补一个最小档位：它表达的是「我要对数概率」，
	// 丢掉等于让一个本协议满足得了的请求落空。取 1 而非更大值——
	// 客户端没说要几个，多取是花上游的算力与响应体。
	if w.TopLogProbs == nil && req.LogProbs != nil && *req.LogProbs {
		n := defaultTopLogProbs
		w.TopLogProbs = &n
	}
	w.Text = encodeText(req.ResponseFormat, req.Verbosity)

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
		// 索要推理签名：store 恒为假（无状态转发），此时上游只在 include
		// 里被明确点名才回 encrypted_content。不要等于永远拿不到签名，
		// 下一轮的推理项就接不上——而摘要不是签名，它不能回传。
		w.Include = withIncluded(w.Include, includeReasoningSig)
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
			// 失败态改写成前缀：本协议的 function_call_output 只有
			// call_id/output 两个键，没有放标记的位置，而丢掉它会让
			// 模型把失败当成功。
			output := joinText(codec.PrefixToolResultError(b.ToolResult, outboundCodec{}.Caps()))
			out = append(out, wireItem{
				Type:   itemFunctionCallOutput,
				CallID: b.ToolResult.ToolUseID,
				// output 恒写键：空文本（纯媒体结果）也得是 ""，
				// 依据见 wireItem.Output 的注释。
				Output: &output,
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
		return wirePart{Type: partInputImage, ImageURL: renderImageURL(b.Media),
			Detail: b.Media.Detail}, true

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

// encodeText 写出 text（结构化输出 + 详略）。两项都空时不写整个 text 对象：
// 空对象对上游是一次多余的表态。
func encodeText(rf *ir.ResponseFormat, verbosity string) *wireText {
	out := &wireText{Verbosity: verbosity}
	switch {
	case rf == nil:
	case rf.Kind == ir.ResponseFormatSchema && rf.Schema != "":
		out.Format = &wireTextFormat{
			Type:   "json_schema",
			Name:   rf.Name,
			Schema: json.RawMessage(rf.Schema),
			Strict: rf.Strict,
		}
	default:
		// schema 形态缺 schema 原文时降级成 json_object：客户端要的最低限度
		// 是「输出是 JSON」，这一点仍能满足。
		out.Format = &wireTextFormat{Type: "json_object"}
	}
	if out.Format == nil && out.Verbosity == "" {
		return nil
	}
	return out
}

// includeReasoningSig 是索要推理签名的 include 项名。
//
// store 为假时上游只在被点名时才回 encrypted_content，而签名是推理
// 跨轮接续的唯一载体（摘要不是签名，它不能回传）。
const includeReasoningSig = "reasoning.encrypted_content"

// defaultTopLogProbs 是客户端只给了 logprobs 开关时补的档位。
//
// 取最小值：客户端说了「要对数概率」但没说要几个，多取是花上游的
// 算力与响应体，而它没要求。
const defaultTopLogProbs = 1

// withIncluded 追加一个 include 项，已存在则原样返回。
//
// 追加而非替换：客户端可能给了别的项，覆盖掉是另一种丢维度。
// 重复项本身也可能被上游拒收。
func withIncluded(include []string, want string) []string {
	for _, v := range include {
		if v == want {
			return include
		}
	}
	// 返回新切片不改入参：入参来自 IR，同一份请求会被两条编码路径
	// 各走一遍，原地追加第二遍就带两项。
	out := make([]string, 0, len(include)+1)
	out = append(out, include...)
	return append(out, want)
}
