package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

type streamDecoder struct {
	// stopped 记录是否已见 message_stop，避免 Finish 重复补发。
	stopped bool
	// notes 记录改写说明，走响应侧诊断通道。
	notes []string
	// droppedUnknown 不认识的事件型/delta 型计数：静默丢弃会让官方新增的
	// 事件型（或代理上游乱发的类型）完全不可见，经 Notes() 报出。
	droppedUnknown int
	// droppedCompaction 服务端压缩回执帧计数：官方类型、语义清楚，但
	// encrypted_content 要求下一轮逐字回传而 IR 没有槽位。与 droppedUnknown
	// 分账——混进去会把「认识但装不下」误报成「不认识」。
	droppedCompaction int
	// droppedBadFrames 外层 JSON 都解不开的坏帧计数：结构损坏而非内容损坏，
	// 跳过续流（SSE 以事件边界自同步，坏一帧不污染后续帧），经 Notes() 报出。
	// 与 droppedUnknown 分账——那是「认识帧但类型不认识」，这是「帧根本解不开」。
	droppedBadFrames int
}

func newStreamDecoder() *streamDecoder { return &streamDecoder{} }

// Notes 实现 codec.StreamNotes。未知帧计数在读取后排干：数字要的是整流结论，
// 重复调用不该把同一批帧再报一遍。
func (d *streamDecoder) Notes() []string {
	notes := d.notes
	if d.droppedUnknown > 0 {
		notes = append(notes, fmt.Sprintf(
			"ignored %d stream event(s) or delta(s) of a type this decoder does not know: the wire carried types outside the documented set, their payload was dropped because no mapping exists", d.droppedUnknown))
		d.droppedUnknown = 0
	}
	if d.droppedCompaction > 0 {
		notes = append(notes, fmt.Sprintf(
			"dropped %d compaction delta(s): the upstream's server-side context compaction receipt must be round-tripped verbatim on the next turn, but no protocol slot carries it through the relay", d.droppedCompaction))
		d.droppedCompaction = 0
	}
	if d.droppedBadFrames > 0 {
		notes = append(notes, codec.BadFrameSkipNote(d.droppedBadFrames))
		d.droppedBadFrames = 0
	}
	return codec.DedupeNotes(notes)
}

func (d *streamDecoder) Feed(event, data string) ([]ir.Event, error) {
	out, split, err := codec.FeedWithSplit(event, data, d.feedOne)
	if split {
		d.notes = append(d.notes, codec.MultipleJSONDocsNote)
	}
	// 整行解不开也拆不出多文档：结构上可忽略的坏帧，计数后吞成无事件无错误，
	// 读流循环据此续流而不是终止整流。内容损坏帧不包裹 ErrSkipFrame，照常上抛。
	if errors.Is(err, codec.ErrSkipFrame) {
		d.droppedBadFrames++
		return nil, nil
	}
	return out, err
}

// isPlaceholderInput 判断开启帧上的 input 是否不含信息。
func isPlaceholderInput(input string) bool {
	switch strings.TrimSpace(input) {
	case "", "null", "{}":
		return true
	default:
		return false
	}
}

func (d *streamDecoder) feedOne(event, data string) ([]ir.Event, error) {
	// ping 帧的 data 可能是空对象，解析它没有意义。
	if event == evPing {
		return []ir.Event{{Type: ir.EvPing}}, nil
	}
	if strings.TrimSpace(data) == "" {
		return nil, nil
	}

	var ev streamEvent
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		// 帧外层解不开：交 ClassifyBadFrame 按「有没有完整文档已解出来」分类——
		// 纯垃圾帧包裹 ErrSkipFrame（Feed 计数跳过续流），残缺多文档行返回内容
		// 损坏错误（fail-fast，除非 FeedWithSplit 能干净拆开）。
		return nil, codec.ClassifyBadFrame(err, data)
	}
	// event 行缺失时以 data 内的 type 为准。
	kind := event
	if kind == "" {
		kind = ev.Type
	}

	switch kind {
	case evMessageStart:
		out := ir.Event{Type: ir.EvMessageStart}
		if ev.Message != nil {
			out.MessageID = ev.Message.ID
			out.Model = ev.Message.Model
			// 上游回显的实际执行档位原值进 IR，跨族映射在出站编码做。
			out.ServiceTier = ev.Message.ServiceTier
			out.Container = decodeContainer(ev.Message.Container)
			u := convertUsage(ev.Message.Usage)
			out.Usage = &u
		}
		return []ir.Event{out}, nil

	case evContentBlockStart:
		block := ir.Block{Type: ir.BlockText}
		if len(ev.BlockRaw) > 0 {
			b, ok, err := decodeRawBlock(ev.BlockRaw)
			if err != nil {
				return nil, ir.NewError(ir.ErrUpstream, 0, "",
					fmt.Sprintf("undecodable content block: %v", err))
			}
			if !ok {
				return nil, nil
			}
			// redacted_thinking 不再丢弃：同族（anthropic 客户端）要逐字收到它，
			// 才能在下一轮原样回传，Anthropic 的续话校验才不会断链。跨族客户端
			// 表达不了，由各自的出站编码器跳过并报有损。涂抹块没有 delta，密文随
			// 块开始全量下发（与 web_search_tool_result.content 同一处置）。
			// 未知块型同理留成不透明块，随块开始全量下发，同族逐字回吐。
			block = b
			// 块开启时的 input 通常是占位空对象，真正入参由后续 input_json_delta
			// 累积；留着占位符会被当成前缀拼进入参，产出非法 JSON。
			if block.ToolUse != nil && isPlaceholderInput(block.ToolUse.Input) {
				block.ToolUse.Input = ""
			}
			// server_tool_use 同款：查询串同样经 input_json_delta 续传。
			if block.ServerToolUse != nil && isPlaceholderInput(block.ServerToolUse.Input) {
				block.ServerToolUse.Input = ""
			}
		}
		out := []ir.Event{{Type: ir.EvBlockStart, Index: ev.Index, Block: &block}}
		// 有实现在开启帧就给出完整入参且不再发增量。IR 约定入参只走增量事件，
		// 所以补发一帧：留在块里会被下游编码器按「开启帧入参必为空」丢掉。
		if block.ToolUse != nil && block.ToolUse.Input != "" {
			input := block.ToolUse.Input
			block.ToolUse.Input = ""
			out = append(out, ir.Event{Type: ir.EvToolInput, Index: ev.Index, Text: input})
		}
		if block.ServerToolUse != nil && block.ServerToolUse.Input != "" {
			input := block.ServerToolUse.Input
			block.ServerToolUse.Input = ""
			out = append(out, ir.Event{Type: ir.EvToolInput, Index: ev.Index, Text: input})
		}
		return out, nil

	case evContentBlockDelta:
		if ev.Delta == nil {
			return nil, nil
		}
		switch ev.Delta.Type {
		case deltaText:
			return []ir.Event{{Type: ir.EvTextDelta, Index: ev.Index, Text: ev.Delta.Text}}, nil
		case deltaInputJSON:
			return []ir.Event{{Type: ir.EvToolInput, Index: ev.Index, Text: ev.Delta.PartialJSON}}, nil
		case deltaThinking:
			return []ir.Event{{Type: ir.EvThinkingDelta, Index: ev.Index, Text: ev.Delta.Thinking}}, nil
		case deltaSignature:
			return []ir.Event{{
				Type:          ir.EvSigDelta,
				Index:         ev.Index,
				Text:          ev.Delta.Signature,
				SignatureFrom: Name,
			}}, nil
		case deltaCitations:
			// 每帧只带一条引用，原文收进 RawMessage：官方五种形态字段互不相同，
			// 逐字段建模会在这一步就把文档类引用的定位字段丢掉。citation 键
			// 缺失或为 null（畸形上游）时 citationsToIR 会跳过非对象元素，
			// 这里再判空避免发一条零事件。
			cs := citationsToIR([]json.RawMessage{ev.Delta.Citation})
			if len(cs) == 0 {
				return nil, nil
			}
			return []ir.Event{{Type: ir.EvCitation, Index: ev.Index, Citations: cs}}, nil
		case deltaCompaction:
			// 服务端压缩回执：encrypted_content 官方要求下一轮逐字回传，
			// IR 没有槽位。这是「认识但装不下」，与「连语义都不认识的型」
			// 分账计数，两条注记各报各的。
			d.droppedCompaction++
			return nil, nil
		default:
			// 未知 delta 类型：跳过而非报错，上游新增字段不该让整个流失败。
			// 但计数经 Notes() 报出——静默丢弃会让新 delta 型完全不可见。
			d.droppedUnknown++
			return nil, nil
		}

	case evContentBlockStop:
		return []ir.Event{{Type: ir.EvBlockStop, Index: ev.Index}}, nil

	case evMessageDelta:
		out := ir.Event{Type: ir.EvMessageDelta}
		if ev.Delta != nil {
			out.StopReason = convertStopReason(ev.Delta.StopReason)
			out.StopSequence = adoptStopSequence(out.StopReason, ev.Delta.StopSequence)
			// 容器回显也可能落在 message_delta 上（官方 Delta.container）。
			out.Container = decodeContainer(ev.Delta.Container)
			// 拒绝档的结构化分类同样只随 message_delta 抵达。
			out.StopDetails = decodeStopDetails(ev.Delta.StopDetails)
		}
		if ev.Usage != nil {
			u := convertUsage(*ev.Usage)
			out.Usage = &u
		}
		return []ir.Event{out}, nil

	case evMessageStop:
		d.stopped = true
		return []ir.Event{{Type: ir.EvMessageStop}}, nil

	case evError:
		return []ir.Event{{Type: ir.EvError, Err: convertError(0, ev.Error)}}, nil

	default:
		// 未知事件型：跳过而非报错（上游新增事件不该让整个流失败），
		// 但计数经 Notes() 报出，静默丢弃会让新事件型完全不可见。
		d.droppedUnknown++
		return nil, nil
	}
}

// Finish 在上游没发 message_stop 就结束流时补一个：
// 下游编码器依赖它闭合流，缺了客户端会一直等。
func (d *streamDecoder) Finish() []ir.Event {
	if d.stopped {
		return nil
	}
	d.stopped = true
	return []ir.Event{{Type: ir.EvMessageStop}}
}

// DecodeResponse 解非流式响应。数据面对上游一律流式，
// 这个路径只在 count_tokens 之类的非流式接口用到。
func DecodeResponse(body []byte) (*ir.Response, error) {
	var w wireResponse
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, ir.NewError(ir.ErrUpstream, 0, "",
			fmt.Sprintf("undecodable response: %v", err))
	}
	out := &ir.Response{
		ID:          w.ID,
		Model:       w.Model,
		StopReason:  convertStopReason(w.StopReason),
		StopDetails: decodeStopDetails(w.StopDetails),
		Usage:       convertUsage(w.Usage),
		ServiceTier: w.ServiceTier,
		Container:   decodeContainer(w.Container),
	}
	out.StopSequence = adoptStopSequence(out.StopReason, w.StopSequence)
	blocks, err := decodeBlocks(w.Content)
	if err != nil {
		return nil, ir.NewError(ir.ErrUpstream, 0, "",
			fmt.Sprintf("undecodable response block: %v", err))
	}
	out.Content = blocks
	return out, nil
}

// DecodeError 把上游错误响应归一成 ir.Error。
func DecodeError(status int, header http.Header, body []byte) *ir.Error {
	var env wireErrorEnvelope
	// 字段类型不匹配不算「什么都没解到」：外层 body 已是合法 JSON，Go 的
	// 解码器记下类型错误后仍会解完其余键——message 写成数字时，解得好的
	// type 不能跟着一起丢，归因靠的是它。
	_ = json.Unmarshal(body, &env)
	if env.Error.Message != "" {
		return codec.WithRetryAfter(codec.WithParam(convertError(status, &env.Error), body), header)
	}
	if env.Error.Type != "" {
		return codec.WithRetryAfter(codec.WithParam(
			codec.SalvagedError(status, body, env.Error.Type), body), header)
	}
	// 上游没按本协议的错误结构回（网关 HTML、兼容层自创字段名之类）：
	// 尽力从任意形状里挖消息，挖不到才回落状态码描述。
	return codec.WithRetryAfter(codec.WithParam(codec.FallbackError(status, body), body), header)
}

func convertError(status int, e *wireError) *ir.Error {
	if e == nil {
		return ir.NewError(codec.KindForStatus(status, ""), status, "", "upstream error without detail")
	}
	// 消息位上可能是被字符串化的下游错误体，取出里面的真消息再归类：
	// 上下文超限的判定要看消息文本，读到一串转义引号就判不出来了。
	msg := codec.RefineMessage(e.Message)
	return ir.NewError(codec.KindFor(status, e.Type, msg), status, e.Type, msg)
}

func convertUsage(u wireUsage) ir.Usage {
	out := ir.Usage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}
	// TTL 明细：上游给了 cache_creation 对象就照实收下。明细已知时
	// 总量若缺席（上游只回细分不回合计的形态），用两档之和补齐——
	// 记账侧只认总量字段，不补就等于把这笔写入用量丢了。
	if u.CacheCreation != nil {
		out.CacheWrite5mTokens = u.CacheCreation.Ephemeral5mInputTokens
		out.CacheWrite1hTokens = u.CacheCreation.Ephemeral1hInputTokens
		out.CacheWriteDetailsKnown = true
		if out.CacheWriteTokens == 0 {
			out.CacheWriteTokens = out.CacheWrite5mTokens + out.CacheWrite1hTokens
		}
	}
	// 托管工具执行次数按次计费，收下才对得了账。
	if u.ServerToolUse != nil {
		out.WebSearchRequests = u.ServerToolUse.WebSearchRequests
		out.WebFetchRequests = u.ServerToolUse.WebFetchRequests
	}
	out.InferenceGeo = u.InferenceGeo
	// beta 迭代用量细分原文透传：message_start / message_delta / 非流式响应
	// 三处都经这里解码，判别式值域在演进不建模。
	out.Iterations = u.Iterations
	return out
}

// decodeStopDetails 拒绝分类进 IR。category/explanation 官方可显式 null，
// null 与缺省同归空串（官方注明二者语义相同）；RawMessage 解不进 string
// 时（上游给了非字符串）保持空串，不让类型怪异连累整个对象。
func decodeStopDetails(sd *wireStopDetails) *ir.StopDetails {
	if sd == nil {
		return nil
	}
	out := &ir.StopDetails{}
	_ = json.Unmarshal(sd.Category, &out.Category)
	_ = json.Unmarshal(sd.Explanation, &out.Explanation)
	return out
}

// adoptStopSequence 只在终止原因确实是停止序列时采纳上游给的那条序列。
//
// 不无条件采纳：按这一维切分输出的客户端拿到一条未触发的序列会切错位置，
// 比拿不到更坏。上游在其他终止原因下带上这个字段（或带一个陈旧值）是
// 我们控制不了的事，能控制的是不把它传下去。
func adoptStopSequence(reason ir.StopReason, seq string) string {
	if reason != ir.StopStopSequence {
		return ""
	}
	return seq
}

func convertStopReason(s string) ir.StopReason {
	switch s {
	case "end_turn":
		return ir.StopEndTurn
	case "max_tokens":
		return ir.StopMaxTokens
	case "stop_sequence":
		return ir.StopStopSequence
	case "tool_use":
		return ir.StopToolUse
	case "refusal":
		return ir.StopContentFilter
	case "pause_turn":
		// 该状态表示回合可以续跑，语义上等同于「没说完」。
		return ir.StopMaxTokens
	case "model_context_window_exceeded":
		// 官方 beta 档：输入占满窗口挤断输出。兜底成 content_filter 会把
		// 截断回答伪装成被拦截，客户端的补救动作（压缩输入）与 max_tokens
		// （抬输出配额）、refusal（换问法）都不同，须单列。
		return ir.StopContextWindow
	case "":
		// 上游没给：留空由聚合层兜底，不能当成被拦截。
		return ""
	default:
		// 未识别的取值按安全侧兜底：把被拦截的回答当正常结束，
		// 客户端会照着不完整的内容继续往下走。
		return ir.StopContentFilter
	}
}

func renderStopReason(s ir.StopReason) string {
	switch s {
	case ir.StopEndTurn:
		return "end_turn"
	case ir.StopMaxTokens:
		return "max_tokens"
	case ir.StopStopSequence:
		return "stop_sequence"
	case ir.StopToolUse:
		return "tool_use"
	case ir.StopContentFilter:
		return "refusal"
	case ir.StopContextWindow:
		// 同族原值带回：外族上游给不出这一档，只有 anthropic 入站的
		// 往返会走到这里。
		return "model_context_window_exceeded"
	case ir.StopMaxMessages, ir.StopSteered:
		// responses 的消息数上限档与用户转向截断档，本协议都无对应值。取
		// max_tokens 而非 end_turn：两者都表示输出不完整，客户端至少不会把
		// 半截结果当成最终答案（end_turn 会），只是上限/成因的维度不同。
		return "max_tokens"
	default:
		return ""
	}
}
