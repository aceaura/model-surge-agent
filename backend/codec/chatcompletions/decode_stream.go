package chatcompletions

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// streamDecoder 把 chat.completion.chunk 序列解成 IR 事件。
//
// 本协议没有块生命周期：正文、推理、工具入参都以「同一处隐式续写」的形式
// 出现在 delta 里。要投影进 IR 的块模型，必须在这里给每个语义槽位分配块索引，
// 并在首次出现时补发 block_start、在流结束时补发 block_stop。
type streamDecoder struct {
	started bool
	// slots 把语义槽位映射到块索引。键是 "text" / "reasoning" / "tool:<n>"。
	slots map[string]int
	// order 记录槽位分配顺序，用于结束时按开启顺序闭合。
	order []int
	next  int

	// stopReason 与 usage 先攒着，到 [DONE] 才发一帧 message_delta。
	// 本协议把它们分散在不同 chunk（finish_reason 一帧、usage 另一帧），
	// 各自发一帧会让下游收到两个 message_delta，Anthropic 客户端不接受。
	stopReason ir.StopReason
	usage      *ir.Usage
	done       bool
}

func newStreamDecoder() *streamDecoder {
	return &streamDecoder{slots: map[string]int{}}
}

func (d *streamDecoder) Feed(_, data string) ([]ir.Event, error) {
	data = strings.TrimSpace(data)
	if data == "" {
		return nil, nil
	}
	if data == doneSentinel {
		return d.finish(), nil
	}

	var chunk wireResponse
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return nil, ir.NewError(ir.ErrUpstream, 0, "",
			fmt.Sprintf("undecodable stream frame: %v", err))
	}
	// 有些实现把错误塞进流内的 chunk 而非独立的 HTTP 状态码。
	var env wireErrorEnvelope
	if json.Unmarshal([]byte(data), &env) == nil && env.Error.Message != "" {
		return []ir.Event{{Type: ir.EvError, Err: convertError(0, &env.Error)}}, nil
	}

	var out []ir.Event
	out = append(out, d.ensureStarted(chunk)...)

	if chunk.Usage != nil {
		u := convertUsage(*chunk.Usage)
		d.usage = &u
	}

	for _, choice := range chunk.Choices {
		// 只处理第一路：IR 是单条响应，n>1 的其余路无处安放。
		if choice.Index != 0 {
			continue
		}
		if choice.Delta != nil {
			events, err := d.decodeDelta(*choice.Delta)
			if err != nil {
				return nil, err
			}
			out = append(out, events...)
		}
		if choice.FinishReason != "" {
			d.stopReason = convertFinishReason(choice.FinishReason)
			// 块在此闭合，但 message_delta 要等 usage 帧，故不在这里发。
			out = append(out, d.closeAll()...)
		}
	}
	return out, nil
}

func (d *streamDecoder) ensureStarted(chunk wireResponse) []ir.Event {
	if d.started {
		return nil
	}
	d.started = true
	return []ir.Event{{Type: ir.EvMessageStart, MessageID: chunk.ID, Model: chunk.Model}}
}

func (d *streamDecoder) decodeDelta(delta wireMessage) ([]ir.Event, error) {
	var out []ir.Event

	if delta.ReasoningContent != "" {
		idx, opened := d.slot("reasoning", ir.BlockThinking)
		out = append(out, opened...)
		out = append(out, ir.Event{Type: ir.EvThinkingDelta, Index: idx, Text: delta.ReasoningContent})
	}

	if len(delta.Content) > 0 {
		blocks, err := decodeContent(delta.Content)
		if err != nil {
			return nil, ir.NewError(ir.ErrUpstream, 0, "",
				fmt.Sprintf("undecodable delta content: %v", err))
		}
		for _, b := range blocks {
			if b.Type != ir.BlockText || b.Text == "" {
				continue
			}
			idx, opened := d.slot("text", ir.BlockText)
			out = append(out, opened...)
			out = append(out, ir.Event{Type: ir.EvTextDelta, Index: idx, Text: b.Text})
		}
	}

	for i, tc := range delta.ToolCalls {
		// index 是本协议拼回分片的唯一依据；缺失时退回数组下标。
		n := i
		if tc.Index != nil {
			n = *tc.Index
		}
		key := fmt.Sprintf("tool:%d", n)
		idx, existed := d.slots[key]
		if !existed {
			// 块开启帧要带上 id 与 name：它们只在首片出现，
			// 攒到后面就无从得知这个调用是哪个工具。
			idx = d.allocate(key)
			out = append(out, ir.Event{
				Type:  ir.EvBlockStart,
				Index: idx,
				Block: &ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
					ID:   tc.ID,
					Name: tc.Function.Name,
				}},
			})
		}
		if tc.Function.Arguments != "" {
			out = append(out, ir.Event{Type: ir.EvToolInput, Index: idx, Text: tc.Function.Arguments})
		}
	}
	return out, nil
}

// slot 取槽位对应的块索引，首次出现时同时产出 block_start。
func (d *streamDecoder) slot(key string, kind ir.BlockType) (int, []ir.Event) {
	if idx, ok := d.slots[key]; ok {
		return idx, nil
	}
	idx := d.allocate(key)
	return idx, []ir.Event{{Type: ir.EvBlockStart, Index: idx, Block: &ir.Block{Type: kind}}}
}

func (d *streamDecoder) allocate(key string) int {
	idx := d.next
	d.next++
	d.slots[key] = idx
	d.order = append(d.order, idx)
	return idx
}

func (d *streamDecoder) closeAll() []ir.Event {
	out := make([]ir.Event, 0, len(d.order))
	for _, idx := range d.order {
		out = append(out, ir.Event{Type: ir.EvBlockStop, Index: idx})
	}
	d.order = nil
	return out
}

// finish 收尾：闭合残留块，补一帧带 stop_reason 与 usage 的 message_delta，
// 再发 message_stop。
func (d *streamDecoder) finish() []ir.Event {
	if d.done {
		return nil
	}
	d.done = true
	out := d.closeAll()
	delta := ir.Event{Type: ir.EvMessageDelta, StopReason: d.stopReason, Usage: d.usage}
	if delta.StopReason == "" {
		delta.StopReason = ir.StopEndTurn
	}
	return append(out, delta, ir.Event{Type: ir.EvMessageStop})
}

// Finish 处理上游没发 [DONE] 就断流的情况。
func (d *streamDecoder) Finish() []ir.Event { return d.finish() }

// DecodeResponse 解非流式响应。数据面对上游一律流式，
// 这条路径只在探测与 count_tokens 之类的接口用到。
func DecodeResponse(body []byte) (*ir.Response, error) {
	var w wireResponse
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, ir.NewError(ir.ErrUpstream, 0, "",
			fmt.Sprintf("undecodable response: %v", err))
	}
	out := &ir.Response{ID: w.ID, Model: w.Model, Content: []ir.Block{}}
	if w.Usage != nil {
		out.Usage = convertUsage(*w.Usage)
	}
	for _, choice := range w.Choices {
		if choice.Index != 0 || choice.Message == nil {
			continue
		}
		out.StopReason = convertFinishReason(choice.FinishReason)
		m := *choice.Message
		if m.ReasoningContent != "" {
			out.Content = append(out.Content, ir.Block{
				Type:     ir.BlockThinking,
				Thinking: &ir.Thinking{Text: m.ReasoningContent, SignatureFrom: Name},
			})
		}
		blocks, err := decodeContent(m.Content)
		if err != nil {
			return nil, ir.NewError(ir.ErrUpstream, 0, "",
				fmt.Sprintf("undecodable response content: %v", err))
		}
		out.Content = append(out.Content, blocks...)
		for _, tc := range m.ToolCalls {
			out.Content = append(out.Content, ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: tc.Function.Arguments,
			}})
		}
	}
	return out, nil
}

func DecodeError(status int, body []byte) *ir.Error {
	var env wireErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil || env.Error.Message == "" {
		return ir.NewError(codec.KindForStatus(status, ""), status, "", codec.StatusMessage(status, body))
	}
	return convertError(status, &env.Error)
}

func convertError(status int, e *wireError) *ir.Error {
	if e == nil {
		return ir.NewError(codec.KindForStatus(status, ""), status, "", "upstream error without detail")
	}
	code := e.Type
	if code == "" {
		code = errorCode(e.Code)
	}
	return ir.NewError(codec.KindForStatus(status, e.Message), status, code, e.Message)
}

// errorCode 取 code 的文本形式：各家有时给字符串有时给数字。
func errorCode(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return strings.Trim(string(raw), `"`)
}

func convertUsage(u wireUsage) ir.Usage {
	out := ir.Usage{
		InputTokens:  u.PromptTokens,
		OutputTokens: u.CompletionTokens,
	}
	// 两种缓存字段名同义，取给出的那个。
	if u.PromptTokensDetails != nil {
		out.CacheReadTokens = u.PromptTokensDetails.CachedTokens
	}
	if out.CacheReadTokens == 0 {
		out.CacheReadTokens = u.PromptCacheHitTokens
	}
	return out
}

func renderUsage(u ir.Usage) wireUsage {
	out := wireUsage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.InputTokens + u.OutputTokens,
	}
	if u.CacheReadTokens > 0 {
		out.PromptTokensDetails = &wirePromptDetails{CachedTokens: u.CacheReadTokens}
	}
	return out
}

func convertFinishReason(s string) ir.StopReason {
	switch s {
	case "stop":
		return ir.StopEndTurn
	case "length":
		return ir.StopMaxTokens
	case "tool_calls", "function_call":
		return ir.StopToolUse
	case "content_filter":
		return ir.StopContentFilter
	default:
		return ""
	}
}

// renderFinishReason 是反向映射。stop_sequence 在本协议里没有单独取值，
// 归到 stop：客户端从 stop 也能正确判断回合结束。
func renderFinishReason(s ir.StopReason) string {
	switch s {
	case ir.StopMaxTokens:
		return "length"
	case ir.StopToolUse:
		return "tool_calls"
	case ir.StopContentFilter:
		return "content_filter"
	default:
		return "stop"
	}
}
