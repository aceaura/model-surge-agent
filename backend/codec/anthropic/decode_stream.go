package anthropic

import (
	"encoding/json"
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
}

func newStreamDecoder() *streamDecoder { return &streamDecoder{} }

// Notes 实现 codec.StreamNotes。
func (d *streamDecoder) Notes() []string { return codec.DedupeNotes(d.notes) }

func (d *streamDecoder) Feed(event, data string) ([]ir.Event, error) {
	out, split, err := codec.FeedWithSplit(event, data, d.feedOne)
	if split {
		d.notes = append(d.notes, codec.MultipleJSONDocsNote)
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
		return nil, ir.NewError(ir.ErrUpstream, 0, "",
			fmt.Sprintf("undecodable stream frame: %v", err))
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
			u := convertUsage(ev.Message.Usage)
			out.Usage = &u
		}
		return []ir.Event{out}, nil

	case evContentBlockStart:
		block := ir.Block{Type: ir.BlockText}
		if ev.Block != nil {
			b, ok, err := decodeBlock(*ev.Block)
			if err != nil {
				return nil, ir.NewError(ir.ErrUpstream, 0, "",
					fmt.Sprintf("undecodable content block: %v", err))
			}
			if !ok || isRedactedThinking(b) {
				// redacted_thinking：整块丢弃，后续该 index 的 delta 也就无处落地，
				// 聚合器会按 delta 类型补块，内容照样保留为普通推理文本。
				// 请求侧留标记是为了报有损，响应侧没有下游要看它。
				return nil, nil
			}
			block = b
			// 块开启时的 input 通常是占位空对象，真正入参由后续 input_json_delta
			// 累积；留着占位符会被当成前缀拼进入参，产出非法 JSON。
			if block.ToolUse != nil && isPlaceholderInput(block.ToolUse.Input) {
				block.ToolUse.Input = ""
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
		default:
			// 未知 delta 类型：跳过而非报错，上游新增字段不该让整个流失败。
			return nil, nil
		}

	case evContentBlockStop:
		return []ir.Event{{Type: ir.EvBlockStop, Index: ev.Index}}, nil

	case evMessageDelta:
		out := ir.Event{Type: ir.EvMessageDelta}
		if ev.Delta != nil {
			out.StopReason = convertStopReason(ev.Delta.StopReason)
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
		ID:         w.ID,
		Model:      w.Model,
		StopReason: convertStopReason(w.StopReason),
		Usage:      convertUsage(w.Usage),
	}
	out.Content = make([]ir.Block, 0, len(w.Content))
	for _, b := range w.Content {
		block, ok, err := decodeBlock(b)
		if err != nil {
			return nil, ir.NewError(ir.ErrUpstream, 0, "",
				fmt.Sprintf("undecodable response block: %v", err))
		}
		if ok && !isRedactedThinking(block) {
			out.Content = append(out.Content, block)
		}
	}
	return out, nil
}

// isRedactedThinking 认出解码期留下的加密推理标记。
// 请求侧留着它是为了报有损，响应侧一律丢：客户端拿到一个空推理块没有用处。
func isRedactedThinking(b ir.Block) bool {
	return b.Type == ir.BlockThinking && b.Thinking != nil && b.Thinking.Redacted
}

// DecodeError 把上游错误响应归一成 ir.Error。
func DecodeError(status int, header http.Header, body []byte) *ir.Error {
	var env wireErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil || env.Error.Message == "" {
		// 上游没按本协议的错误结构回（网关 HTML、兼容层自创字段名之类）：
		// 尽力从任意形状里挖消息，挖不到才回落状态码描述。
		return codec.WithRetryAfter(codec.WithParam(codec.FallbackError(status, body), body), header)
	}
	return codec.WithRetryAfter(codec.WithParam(convertError(status, &env.Error), body), header)
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
	return ir.Usage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}
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
	default:
		return ""
	}
}
