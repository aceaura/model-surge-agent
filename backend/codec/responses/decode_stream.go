package responses

import (
	"encoding/json"
	"fmt"
	"net/http"
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
	// serviceTier 是上游回的执行档位，created 与 completed 两帧都可能带。
	serviceTier string
	usage       *ir.Usage
	// refused 记录流里出现过拒答 part。
	//
	// 必须在流里记而不是只看收尾帧：收尾帧的 response 对象可能不带
	// 完整 output（上游实现不一），那时只有中途的 part 帧见过拒答。
	refused bool
	// notes 记录改写说明，走响应侧诊断通道。
	notes []string
}

// slot 把「两级序号构成的键」绑到分配给它的 IR 块索引。
type slot struct {
	key   string
	index int
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

func (d *streamDecoder) feedOne(event, data string) ([]ir.Event, error) {
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
			if ev.Part.Type == partRefusal {
				d.refused = true
			}
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
		if ev.Type == evRefusalDelta {
			// part 开启帧可能整个缺席（上游只发 delta），所以两处都要记。
			d.refused = true
		}
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
		out.ServiceTier = ev.Response.ServiceTier
		d.serviceTier = ev.Response.ServiceTier
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
		out = append(out, ir.Event{
			Type:  ir.EvBlockStart,
			Index: idx,
			Block: &ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
				ID:   ev.Item.CallID,
				Name: ev.Item.Name,
			}},
		})
		// 有实现在开启帧就给出完整 arguments 且不再发增量，当一次 delta 发出。
		if ev.Item.Arguments != "" {
			out = append(out, ir.Event{Type: ir.EvToolInput, Index: idx, Text: ev.Item.Arguments})
		}
		return out, nil
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
		if ev.Response.ServiceTier != "" {
			d.serviceTier = ev.Response.ServiceTier
		}
	}
	// 流里见过拒答就改判，除非收尾帧已经给出一个非正常结束的原因——
	// 那是上游更明确的表态（比如同时被截断）。
	if d.refused && (d.stopReason == "" || d.stopReason == ir.StopEndTurn) {
		d.stopReason = ir.StopContentFilter
	}
	delta := ir.Event{Type: ir.EvMessageDelta, StopReason: d.stopReason, Usage: d.usage,
		ServiceTier: d.serviceTier}
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
		ID:          w.ID,
		Model:       w.Model,
		StopReason:  stopReasonFor(&w),
		Content:     []ir.Block{},
		ServiceTier: w.ServiceTier,
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

func DecodeError(status int, header http.Header, body []byte) *ir.Error {
	return codec.WithRetryAfter(decodeErrorBody(status, body), header)
}

func decodeErrorBody(status int, body []byte) *ir.Error {
	var env wireErrorEnvelope
	if err := json.Unmarshal(body, &env); err == nil && env.Error.Message != "" {
		return codec.WithParam(convertError(status, &env.Error), body)
	}
	// 有些错误是裸的 response 对象，错误挂在 error 字段上。
	var resp wireResponse
	if err := json.Unmarshal(body, &resp); err == nil && resp.Error != nil {
		return codec.WithParam(convertError(status, resp.Error), body)
	}
	// 两种规范形状都不匹配：尽力从任意形状里挖消息，挖不到才回落状态码描述。
	return codec.WithParam(codec.FallbackError(status, body), body)
}

func convertError(status int, e *wireError) *ir.Error {
	if e == nil {
		return ir.NewError(codec.KindForStatus(status, ""), status, "", "upstream error without detail")
	}
	code := e.Code
	if code == "" {
		code = e.Type
	}
	// 消息位上可能是被字符串化的下游错误体，取出里面的真消息再归类：
	// 上下文超限的判定要看消息文本，读到一串转义引号就判不出来了。
	msg := codec.RefineMessage(e.Message)
	out := ir.NewError(codec.KindFor(status, code, msg), status, code, msg)
	out.Param = e.Param
	return out
}

func streamError(ev wireStreamEvent) *ir.Error {
	return convertError(0, &wireError{Code: ev.Code, Message: ev.Message, Param: ev.Param})
}

func convertUsage(u wireUsage) ir.Usage {
	out := ir.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens}
	if u.InputTokensDetails != nil {
		out.CacheReadTokens = u.InputTokensDetails.CachedTokens
	}
	// 本协议的 output_tokens 已含推理，IR 同口径，故只记维度不做扣减。
	if u.OutputTokensDetails != nil {
		out.ReasoningTokens = u.OutputTokensDetails.ReasoningTokens
	}
	// 本协议的 input_tokens 含缓存命中，而 IR 的 InputTokens 定义为
	// 不含缓存的新鲜输入，故减去。上游数字不自洽时钳到 0，不出负数。
	out.InputTokens -= out.CacheReadTokens
	if out.InputTokens < 0 {
		out.InputTokens = 0
	}
	return out
}

func renderUsage(u ir.Usage) wireUsage {
	// 加回缓存命中：本协议的客户端期望 input_tokens 是输入总量。
	input := u.InputTokens + u.CacheReadTokens
	out := wireUsage{
		InputTokens:  input,
		OutputTokens: u.OutputTokens,
		TotalTokens:  input + u.OutputTokens,
	}
	if u.CacheReadTokens > 0 {
		out.InputTokensDetails = &wireInputDetails{CachedTokens: u.CacheReadTokens}
	}
	// 本协议表达得了推理维度，如实写出，让客户端看得见推理占比。
	if u.ReasoningTokens > 0 {
		out.OutputTokensDetails = &wireOutputDetails{ReasoningTokens: u.ReasoningTokens}
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
	if r.IncompleteDetails != nil && r.IncompleteDetails.Reason != "" {
		switch r.IncompleteDetails.Reason {
		case "max_output_tokens":
			return ir.StopMaxTokens
		default:
			// 上游已明说这次没完成，未识别的原因按安全侧兜底。
			// 落到下面的分支会被判成正常结束，客户端就不知道内容是残的。
			return ir.StopContentFilter
		}
	}
	// 拒答的判定排在工具调用之前：两者同时出现时拒答是更重要的那一维。
	// 判成 tool_use 会让客户端去执行工具，而模型其实是拒绝了。
	//
	// 排在 incomplete_details 之后：那是上游对「为什么没完成」的明确
	// 表态，比我方从 part 类型推断出的更可靠。
	if outputHasRefusal(r) {
		return ir.StopContentFilter
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

// outputHasRefusal 判断输出里有没有拒答 part。
//
// 上游用一个独立的 part 类型表达拒答，而本协议的 status 仍是 completed，
// 于是「模型拒绝回答」与「模型答完了」在 status 上看不出差别。
// anthropic 入站有原生的 refusal 终止原因（见其 convertStopReason），
// 两边不一致会让同一次拒答在不同入站协议上得到不同的 stop_reason。
func outputHasRefusal(r *wireResponse) bool {
	for _, item := range r.Output {
		if item.Type != itemMessage || len(item.Content) == 0 {
			continue
		}
		var parts []wirePart
		if err := json.Unmarshal(item.Content, &parts); err != nil {
			// content 是字符串形态时解不成 part 数组，那种形态里没有
			// 拒答的表达位置，不是错误。
			continue
		}
		for _, p := range parts {
			if p.Type == partRefusal {
				return true
			}
		}
	}
	return false
}

// renderStatus 是反向映射：出站为客户端合成 response 对象时用。
//
// 本协议没有独立的终止原因字段，工具调用由 output 里的 function_call 条目
// 表达，所以 tool_use 必须回 completed——标成 incomplete 会让客户端
// 以为回答被截断而不去执行工具，整个工具回合就断在这里。
//
// stop_sequence 在本协议里无对应取值，completed 是最接近的表达：
// 命中停止序列确实是一次正常收尾，只是「因何而停」这一位表达不出来。
func renderStatus(s ir.StopReason) (status string, incomplete *wireIncomplete) {
	switch s {
	case ir.StopMaxTokens:
		return "incomplete", &wireIncomplete{Reason: "max_output_tokens"}
	case ir.StopContentFilter:
		return "incomplete", &wireIncomplete{Reason: "content_filter"}
	case ir.StopEndTurn, ir.StopToolUse, ir.StopStopSequence, "":
		return "completed", nil
	default:
		// 认不出的 IR 取值不按正常结束报，理由同各 convert 的 default 分支。
		return "incomplete", &wireIncomplete{Reason: "content_filter"}
	}
}
