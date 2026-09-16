package responses

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// streamDecoder 把 response.* 帧序列解成 IR 事件。
//
// 本协议用 (output_index, content_index) 两级序号定位增量，而 IR 只有一级
// 块索引，所以这里维护一张映射表，按两级序号首次出现的顺序分配块索引。
// 不能直接拿 output_index 当块索引：它在同一条目内多 part 时会重复，
// 且本协议不保证连续（内建工具条目会占号）。
type streamDecoder struct {
	started bool
	done    bool
	// open 是仍开着的块，按分配顺序排列。用切片而非 map 是因为
	// 闭合要按开启顺序，且 itemDone 要按键前缀成批查找。
	open []slot
	next int

	// stopReason 与 usage 只在 completed/incomplete 帧出现，
	// 到那时一并发出一帧 message_delta。
	stopReason ir.StopReason
	usage      *ir.Usage
}

// slot 把「两级序号构成的键」绑到分配给它的 IR 块索引。
type slot struct {
	key   string
	index int
}

func newStreamDecoder() *streamDecoder { return &streamDecoder{} }

func (d *streamDecoder) Feed(event, data string) ([]ir.Event, error) {
	if strings.TrimSpace(data) == "" {
		return nil, nil
	}
	var ev wireStreamEvent
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
	case evCreated, evInProgress:
		return d.start(ev), nil

	case evOutputItemAdded:
		return d.itemAdded(ev)

	case evContentPartAdded:
		// message 条目的 part：文本块在这里开启。函数调用与推理条目
		// 不发这个帧，它们在 output_item.added 时就已开块。
		if ev.Part == nil {
			return nil, nil
		}
		switch ev.Part.Type {
		case partOutputText, partRefusal:
			idx, opened := d.slot(partKey(ev.OutputIndex, ev.ContentIndex), ir.BlockText)
			out := d.start(ev)
			out = append(out, opened...)
			// part 开启帧可能已带完整文本（非增量实现），带了就当一次 delta 发出。
			if text := partText(ev.Part); text != "" {
				out = append(out, ir.Event{Type: ir.EvTextDelta, Index: idx, Text: text})
			}
			return out, nil
		default:
			return nil, nil
		}

	case evOutputTextDelta, evRefusalDelta:
		idx, opened := d.slot(partKey(ev.OutputIndex, ev.ContentIndex), ir.BlockText)
		out := append(d.start(ev), opened...)
		return append(out, ir.Event{Type: ir.EvTextDelta, Index: idx, Text: ev.Delta}), nil

	case evFunctionArgsDelta:
		// 函数调用条目只有一个 arguments 流，用 output_index 单独成键。
		idx, opened := d.slot(callKey(ev.OutputIndex), ir.BlockToolUse)
		out := append(d.start(ev), opened...)
		return append(out, ir.Event{Type: ir.EvToolInput, Index: idx, Text: ev.Delta}), nil

	case evReasoningSummaryText, evReasoningTextDelta:
		// 推理摘要按 summary_index 分段，各段是同一块的续写：
		// IR 的一个 thinking 块承载全部段落，段间不需要边界。
		idx, opened := d.slot(reasoningKey(ev.OutputIndex), ir.BlockThinking)
		out := append(d.start(ev), opened...)
		return append(out, ir.Event{Type: ir.EvThinkingDelta, Index: idx, Text: ev.Delta}), nil

	case evOutputItemDone:
		return d.itemDone(ev), nil

	case evContentPartDone, evOutputTextDone, evFunctionArgsDone:
		// 这些帧只是「该 part 已完整」的确认，块闭合统一在 output_item.done 做。
		// 在这里也闭合会产出重复的 block_stop。
		return nil, nil

	case evCompleted, evIncomplete, evFailed:
		return d.complete(ev), nil

	case evError:
		return []ir.Event{{Type: ir.EvError, Err: streamError(ev)}}, nil

	default:
		return nil, nil
	}
}

func (d *streamDecoder) start(ev wireStreamEvent) []ir.Event {
	if d.started {
		return nil
	}
	d.started = true
	out := ir.Event{Type: ir.EvMessageStart}
	if ev.Response != nil {
		out.MessageID = ev.Response.ID
		out.Model = ev.Response.Model
	}
	return []ir.Event{out}
}

// itemAdded 处理条目开启。函数调用必须在这里开块：id 与 name 只在这一帧出现，
// 后续的 arguments 增量帧不带它们。
func (d *streamDecoder) itemAdded(ev wireStreamEvent) ([]ir.Event, error) {
	out := d.start(ev)
	if ev.Item == nil {
		return out, nil
	}
	switch ev.Item.Type {
	case itemFunctionCall:
		key := callKey(ev.OutputIndex)
		if _, exists := d.lookup(key); exists {
			return out, nil
		}
		idx := d.allocate(key)
		return append(out, ir.Event{
			Type:  ir.EvBlockStart,
			Index: idx,
			Block: &ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
				ID:   ev.Item.CallID,
				Name: ev.Item.Name,
			}},
		}), nil
	case itemReasoning:
		key := reasoningKey(ev.OutputIndex)
		if _, exists := d.lookup(key); exists {
			return out, nil
		}
		idx := d.allocate(key)
		block := ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{SignatureFrom: Name}}
		if ev.Item.EncryptedContent != "" {
			block.Thinking.Signature = ev.Item.EncryptedContent
		}
		return append(out, ir.Event{Type: ir.EvBlockStart, Index: idx, Block: &block}), nil
	default:
		// message 条目的块在 content_part.added 才开：一个条目可能有多个 part，
		// 每个 part 是独立的 IR 块。
		return out, nil
	}
}

// itemDone 闭合该条目下的全部块。
func (d *streamDecoder) itemDone(ev wireStreamEvent) []ir.Event {
	prefix := fmt.Sprintf("%d:", ev.OutputIndex)
	var out []ir.Event
	for _, s := range d.open {
		if strings.HasPrefix(s.key, prefix) {
			out = append(out, ir.Event{Type: ir.EvBlockStop, Index: s.index})
		}
	}
	d.forget(prefix)
	return out
}

// complete 收尾：闭合残留块，发 message_delta 与 message_stop。
func (d *streamDecoder) complete(ev wireStreamEvent) []ir.Event {
	if d.done {
		return nil
	}
	d.done = true
	out := d.start(ev)
	out = append(out, d.closeAll()...)

	if ev.Response != nil {
		if ev.Response.Usage != nil {
			u := convertUsage(*ev.Response.Usage)
			d.usage = &u
		}
		d.stopReason = stopReasonFor(ev.Response)
	}
	delta := ir.Event{Type: ir.EvMessageDelta, StopReason: d.stopReason, Usage: d.usage}
	if delta.StopReason == "" {
		delta.StopReason = ir.StopEndTurn
	}
	// failed 帧带错误：先把错误交出去，再收束流。
	if ev.Type == evFailed && ev.Response != nil && ev.Response.Error != nil {
		out = append(out, ir.Event{
			Type: ir.EvError,
			Err:  convertError(0, ev.Response.Error),
		})
	}
	return append(out, delta, ir.Event{Type: ir.EvMessageStop})
}

func (d *streamDecoder) closeAll() []ir.Event {
	out := make([]ir.Event, 0, len(d.open))
	for _, s := range d.open {
		out = append(out, ir.Event{Type: ir.EvBlockStop, Index: s.index})
	}
	d.open = nil
	return out
}

// Finish 处理上游没发终止帧就断流的情况。
func (d *streamDecoder) Finish() []ir.Event {
	if d.done {
		return nil
	}
	return d.complete(wireStreamEvent{})
}

// slot 取键对应的块索引，首次出现时同时产出 block_start。
func (d *streamDecoder) slot(key string, kind ir.BlockType) (int, []ir.Event) {
	if idx, ok := d.lookup(key); ok {
		return idx, nil
	}
	idx := d.allocate(key)
	block := ir.Block{Type: kind}
	switch kind {
	case ir.BlockThinking:
		block.Thinking = &ir.Thinking{SignatureFrom: Name}
	case ir.BlockToolUse:
		block.ToolUse = &ir.ToolUse{}
	}
	return idx, []ir.Event{{Type: ir.EvBlockStart, Index: idx, Block: &block}}
}

func (d *streamDecoder) lookup(key string) (int, bool) {
	for _, s := range d.open {
		if s.key == key {
			return s.index, true
		}
	}
	return 0, false
}

func (d *streamDecoder) allocate(key string) int {
	idx := d.next
	d.next++
	d.open = append(d.open, slot{key: key, index: idx})
	return idx
}

// forget 移除该条目下的槽位，让 closeAll 不再重复闭合它们。
func (d *streamDecoder) forget(prefix string) {
	kept := d.open[:0]
	for _, s := range d.open {
		if strings.HasPrefix(s.key, prefix) {
			continue
		}
		kept = append(kept, s)
	}
	d.open = kept
}

// 槽位键都以 output_index 开头，itemDone 靠这个前缀找到该条目的全部块。
func partKey(output, content int) string { return fmt.Sprintf("%d:part:%d", output, content) }
func callKey(output int) string          { return fmt.Sprintf("%d:call", output) }
func reasoningKey(output int) string     { return fmt.Sprintf("%d:reasoning", output) }

func partText(p *wirePart) string {
	if p.Text != "" {
		return p.Text
	}
	return p.Refusal
}

// DecodeResponse 解非流式响应。数据面对上游一律流式，
// 这条路径只在探测之类的接口用到。
func DecodeResponse(body []byte) (*ir.Response, error) {
	var w wireResponse
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, ir.NewError(ir.ErrUpstream, 0, "",
			fmt.Sprintf("undecodable response: %v", err))
	}
	out := &ir.Response{
		ID:         w.ID,
		Model:      w.Model,
		StopReason: stopReasonFor(&w),
		Content:    []ir.Block{},
	}
	if w.Usage != nil {
		out.Usage = convertUsage(*w.Usage)
	}
	for _, item := range w.Output {
		switch item.Type {
		case itemMessage:
			blocks, err := decodeContent(item.Content)
			if err != nil {
				return nil, ir.NewError(ir.ErrUpstream, 0, "",
					fmt.Sprintf("undecodable output content: %v", err))
			}
			out.Content = append(out.Content, blocks...)
		case itemFunctionCall:
			out.Content = append(out.Content, ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
				ID:    item.CallID,
				Name:  item.Name,
				Input: item.Arguments,
			}})
		case itemReasoning:
			out.Content = append(out.Content, ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{
				Text:          joinSummary(item.Summary),
				Signature:     item.EncryptedContent,
				SignatureFrom: Name,
			}})
		}
	}
	return out, nil
}

func DecodeError(status int, body []byte) *ir.Error {
	var env wireErrorEnvelope
	if err := json.Unmarshal(body, &env); err == nil && env.Error.Message != "" {
		return convertError(status, &env.Error)
	}
	// 有些错误是裸的 response 对象，错误挂在 error 字段上。
	var resp wireResponse
	if err := json.Unmarshal(body, &resp); err == nil && resp.Error != nil {
		return convertError(status, resp.Error)
	}
	return ir.NewError(codec.KindForStatus(status, ""), status, "", codec.StatusMessage(status, body))
}

func convertError(status int, e *wireError) *ir.Error {
	if e == nil {
		return ir.NewError(codec.KindForStatus(status, ""), status, "", "upstream error without detail")
	}
	code := e.Code
	if code == "" {
		code = e.Type
	}
	return ir.NewError(codec.KindForStatus(status, e.Message), status, code, e.Message)
}

func streamError(ev wireStreamEvent) *ir.Error {
	return convertError(0, &wireError{Code: ev.Code, Message: ev.Message, Param: ev.Param})
}

func convertUsage(u wireUsage) ir.Usage {
	out := ir.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens}
	if u.InputTokensDetails != nil {
		out.CacheReadTokens = u.InputTokensDetails.CachedTokens
	}
	return out
}

func renderUsage(u ir.Usage) wireUsage {
	out := wireUsage{
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		TotalTokens:  u.InputTokens + u.OutputTokens,
	}
	if u.CacheReadTokens > 0 {
		out.InputTokensDetails = &wireInputDetails{CachedTokens: u.CacheReadTokens}
	}
	return out
}

// stopReasonFor 推终止原因。本协议没有单独的 stop_reason 字段：
// 正常结束看 status，截断看 incomplete_details.reason，
// 工具调用则要看 output 里有没有 function_call 条目。
func stopReasonFor(r *wireResponse) ir.StopReason {
	if r == nil {
		return ""
	}
	if r.IncompleteDetails != nil {
		switch r.IncompleteDetails.Reason {
		case "max_output_tokens":
			return ir.StopMaxTokens
		case "content_filter":
			return ir.StopContentFilter
		}
	}
	for _, item := range r.Output {
		if item.Type == itemFunctionCall {
			return ir.StopToolUse
		}
	}
	if r.Status == "incomplete" {
		return ir.StopMaxTokens
	}
	return ir.StopEndTurn
}

// renderStatus 是反向映射：出站为客户端合成 response 对象时用。
func renderStatus(s ir.StopReason) (status string, incomplete *wireIncomplete) {
	switch s {
	case ir.StopMaxTokens:
		return "incomplete", &wireIncomplete{Reason: "max_output_tokens"}
	case ir.StopContentFilter:
		return "incomplete", &wireIncomplete{Reason: "content_filter"}
	default:
		return "completed", nil
	}
}
