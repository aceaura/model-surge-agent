package anthropic

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

type streamDecoder struct {
	// stopped 记录是否已见 message_stop，避免 Finish 重复补发。
	stopped bool
}

func newStreamDecoder() *streamDecoder { return &streamDecoder{} }

func (d *streamDecoder) Feed(event, data string) ([]ir.Event, error) {
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
			if !ok {
				// redacted_thinking：整块丢弃，后续该 index 的 delta 也就无处落地，
				// 聚合器会按 delta 类型补块，内容照样保留为普通推理文本。
				return nil, nil
			}
			block = b
			// 块开启时的 input 是占位空对象，真正入参由后续 input_json_delta
			// 累积。留着它会被当成前缀拼进入参，产出非法 JSON。
			if block.ToolUse != nil {
				block.ToolUse.Input = ""
			}
		}
		return []ir.Event{{Type: ir.EvBlockStart, Index: ev.Index, Block: &block}}, nil

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
			return []ir.Event{{Type: ir.EvSigDelta, Index: ev.Index, Text: ev.Delta.Signature}}, nil
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
		if ok {
			out.Content = append(out.Content, block)
		}
	}
	return out, nil
}

// DecodeError 把上游错误响应归一成 ir.Error。
func DecodeError(status int, body []byte) *ir.Error {
	var env wireErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil || env.Error.Message == "" {
		// 上游返回了非预期格式（网关 HTML 页面之类），按状态码归类。
		return ir.NewError(kindForStatus(status, ""), status, "", statusMessage(status, body))
	}
	return convertError(status, &env.Error)
}

func convertError(status int, e *wireError) *ir.Error {
	if e == nil {
		return ir.NewError(kindForStatus(status, ""), status, "", "upstream error without detail")
	}
	return ir.NewError(kindForStatus(status, e.Message), status, e.Type, e.Message)
}

// kindForStatus 按状态码归类，并对 400 额外看消息内容：
// 上下文超限与普通参数错误都是 400，但前者不该记作目标失败。
func kindForStatus(status int, message string) ir.ErrorKind {
	switch {
	case status == http.StatusTooManyRequests:
		return ir.ErrRateLimit
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return ir.ErrAuth
	case status == http.StatusNotFound:
		return ir.ErrNotFound
	case status == http.StatusRequestEntityTooLarge:
		return ir.ErrContextExceeded
	case status == http.StatusBadRequest:
		if isContextOverflow(message) {
			return ir.ErrContextExceeded
		}
		return ir.ErrInvalidRequest
	case status >= 500:
		return ir.ErrUpstream
	case status == 0:
		return ir.ErrUpstream
	default:
		return ir.ErrInvalidRequest
	}
}

// contextOverflowMarkers 是各家表达「输入太长」的说法。
// 没有统一错误码，只能匹配消息文本。
var contextOverflowMarkers = []string{
	"context length",
	"context_length",
	"context window",
	"maximum context",
	"too many tokens",
	"prompt is too long",
	"input length",
	"reduce the length",
	"exceeds the maximum",
}

func isContextOverflow(message string) bool {
	m := strings.ToLower(message)
	for _, marker := range contextOverflowMarkers {
		if strings.Contains(m, marker) {
			return true
		}
	}
	return false
}

func statusMessage(status int, body []byte) string {
	const maxLen = 512
	text := strings.TrimSpace(string(body))
	if text == "" {
		return fmt.Sprintf("upstream returned %d", status)
	}
	if len(text) > maxLen {
		text = text[:maxLen]
	}
	return fmt.Sprintf("upstream returned %d: %s", status, text)
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
	default:
		return ""
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
