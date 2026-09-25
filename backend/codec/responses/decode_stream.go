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
	// closed 是已闭合块的槽位键。part 级 done 帧可能在块闭合后才到
	// （迟到的终态帧），那时回补会把内容追加到已定稿的条目上。
	closed map[string]struct{}
	// sawError 记录流里已经交出过错误。上游的真实序列是
	// error → response.failed（sub2api 夹具可证），错误详情只在裸 error
	// 帧里；failed 帧再补一份空壳错误会让客户端看到两次报错。
	// 反过来，见过错误后即使上游继续发 completed 也不能伪装成正常结束。
	sawError bool
	// customKinds 记「这个 output_index 的调用条目是 custom_tool_call」。
	// 增量帧不带条目类型（delta 帧只有 output_index 与文本），而块开启
	// 可能发生在增量帧上（上游漏发 added 帧），所以两处都要能查到形态，
	// 且以先见者为准——同一序号不会出现两种形态的条目。
	customKinds map[int]bool
	// hostedItems 是本变换没有映射、整项丢弃的托管输出项数
	//（web_search_call 之类）。只在收尾时生成一条注记：数字要的是整流
	// 的结论，逐帧生成会让每帧数字不同、去重挡不住。
	hostedItems int
	// droppedProgress 被忽略的托管调用进度帧数（*_call.* 的
	// in_progress/searching/completed、partial_image、mcp_call_arguments 与
	// code_interpreter 的增量等）：终态内容随 output_item.done 完整到达，
	// 进度帧本身没有 IR 事件对应物，同族转发也只能丢——计数经 Notes()
	// 报出，不再静默。累计口径与 hostedItems 相同。
	droppedProgress int
	// droppedUnknown 本仓不认识的事件型计数（上游新增事件、畸形 type 等）：
	// 与进度帧分账——前者是「已知但无对应物的过程信号」，这里是「解码器
	// 连语义都不知道的帧」，混在一起会让上游新能力静默蒸发看不出来。
	droppedUnknown int
	// droppedLogprobs 携带 logprobs 的 output_text part 数：逐 token 概率
	// 没有 IR 槽位，计数经 Notes() 报出。
	droppedLogprobs int
}

// slot 把「两级序号构成的键」绑到分配给它的 IR 块索引。
type slot struct {
	key   string
	index int
	// text 是已发出的正文/思考前缀。done 帧携带的是完整终态而不是新一份
	// 内容，回补判据要靠这份账：只补尚未发出的后缀。
	text string
	// sig 记住这个块已经拿到过的推理签名。多数上游只在 output_item.done 上
	// 挂 encrypted_content，少数在 added 上就给；两处都给时不能都发——聚合器
	// 对签名增量是累加的，第二条会把两份密文拼成一段无法解密的垃圾。
	sig string
}

func newStreamDecoder() *streamDecoder {
	return &streamDecoder{closed: map[string]struct{}{}, customKinds: map[int]bool{}}
}

// Notes 实现 codec.StreamNotes。
func (d *streamDecoder) Notes() []string {
	notes := d.notes
	if d.hostedItems > 0 {
		notes = append(notes, codec.HostedOutputItemsNote(d.hostedItems))
		d.hostedItems = 0
	}
	if d.droppedProgress > 0 {
		notes = append(notes, fmt.Sprintf(
			"ignored %d hosted-call progress frame(s): terminal content still arrives with each item's done frame, only the progress signal itself has no counterpart in this conversion", d.droppedProgress))
		d.droppedProgress = 0
	}
	if d.droppedUnknown > 0 {
		notes = append(notes, fmt.Sprintf(
			"ignored %d stream event(s) of a type this decoder does not know: the upstream sent event types outside the documented set, their payload was dropped because no mapping exists", d.droppedUnknown))
		d.droppedUnknown = 0
	}
	if d.droppedLogprobs > 0 {
		notes = append(notes, codec.LogProbsDropNote(d.droppedLogprobs))
		d.droppedLogprobs = 0
	}
	return codec.DedupeNotes(notes)
}

func (d *streamDecoder) Feed(event, data string) ([]ir.Event, error) {
	out, split, err := codec.FeedWithSplit(event, data, d.feedOne)
	if split {
		d.notes = append(d.notes, codec.MultipleJSONDocsNote)
	}
	return out, err
}

// isProgressEvent 已知但无 IR 对应物的进度信号：response.queued、
// 托管调用的生命周期帧（*_call.in_progress/searching/completed 等）与它们的
// 增量帧（mcp_call_arguments.*、code_interpreter_call_code.*）、
// mcp_list_tools.* 与 partial_image。终态内容都随 output_item.done 到达。
// response.created/in_progress 不在此列：本仓拿它们兜底开启消息。
func isProgressEvent(typ string) bool {
	if typ == "response.queued" {
		return true
	}
	return strings.Contains(typ, "_call.") || strings.Contains(typ, "_call_") ||
		strings.Contains(typ, "_list_tools.")
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
			kind := ir.BlockText
			if ev.Part.Type == partRefusal {
				// 拒绝正文自成一块：并入文本块会让客户端把拒绝渲染成
				// 普通回答，只凭终止原因无法区分。
				d.refused = true
				kind = ir.BlockRefusal
			}
			idx, opened := d.slot(partKey(ev.OutputIndex, ev.ContentIndex), kind)
			out := d.start(ev)
			out = append(out, opened...)
			// part 开启帧可能已带完整文本（非增量实现），带了就当一次 delta 发出。
			if text := partText(ev.Part); text != "" {
				d.accText(idx, text)
				out = append(out, ir.Event{Type: ir.EvTextDelta, Index: idx, Text: text})
			}
			return out, nil
		default:
			return nil, nil
		}

	case evOutputTextDelta, evRefusalDelta:
		kind := ir.BlockText
		if ev.Type == evRefusalDelta {
			// part 开启帧可能整个缺席（上游只发 delta），所以两处都要记。
			d.refused = true
			kind = ir.BlockRefusal
		}
		idx, opened := d.slot(partKey(ev.OutputIndex, ev.ContentIndex), kind)
		out := append(d.start(ev), opened...)
		d.accText(idx, ev.Delta)
		return append(out, ir.Event{Type: ir.EvTextDelta, Index: idx, Text: ev.Delta}), nil

	case evOutputTextAnnotationAdded:
		// 标注增量帧。此前它与 output_text.delta 并成一个 case，而它没有
		// delta 字段，整帧被静默丢掉——web 搜索回答的来源标注全部消失。
		//
		// 只挂到已存在的 part 槽位（lookup 不分配）：标注到达时正文必然
		// 已开启，没开说明序号对不上，为一个不存在的 part 分配块索引
		// 会让客户端多出一个空文本块。
		cs := decodeAnnotations([]annotation{*orEmptyAnnotation(ev.Annotation)})
		if len(cs) == 0 {
			return nil, nil
		}
		idx, ok := d.lookup(partKey(ev.OutputIndex, ev.ContentIndex))
		if !ok {
			return nil, nil
		}
		return []ir.Event{{Type: ir.EvCitation, Index: idx, Citations: cs}}, nil

	case evFunctionArgsDelta:
		// 函数调用条目只有一个 arguments 流，用 output_index 单独成键。
		idx, opened := d.slotCall(ev.OutputIndex)
		out := append(d.start(ev), opened...)
		d.accText(idx, ev.Delta)
		return append(out, ir.Event{Type: ir.EvToolInput, Index: idx, Text: ev.Delta}), nil

	case evFunctionArgsDone:
		// 终态帧携带完整参数而不是新一份：只发终态的上游靠这一帧拿到
		// 全部入参，已发 delta 的前缀不得重复，分叉也不能追加成畸形 JSON。
		return d.backfill(callKey(ev.OutputIndex), ir.BlockToolUse,
			ev.Arguments, ir.EvToolInput), nil

	case evCustomToolInputDelta:
		// custom_tool_call 的入参增量：帧结构与 function_call 的 arguments
		// 增量相同，只是键名从 arguments 换成 input、事件名独立。
		d.customKinds[ev.OutputIndex] = true
		idx, opened := d.slotCall(ev.OutputIndex)
		out := append(d.start(ev), opened...)
		d.accText(idx, ev.Delta)
		return append(out, ir.Event{Type: ir.EvToolInput, Index: idx, Text: ev.Delta}), nil

	case evCustomToolInputDone:
		// 终态在 input 键上；口径同 evFunctionArgsDone。
		return d.backfill(callKey(ev.OutputIndex), ir.BlockToolUse,
			ev.Input, ir.EvToolInput), nil

	case evReasoningSummaryText, evReasoningTextDelta:
		// 推理摘要按 summary_index 分段，各段是同一块的续写：
		// IR 的一个 thinking 块承载全部段落，段间不需要边界。
		idx, opened := d.slot(reasoningKey(ev.OutputIndex), ir.BlockThinking)
		out := append(d.start(ev), opened...)
		d.accText(idx, ev.Delta)
		return append(out, ir.Event{Type: ir.EvThinkingDelta, Index: idx, Text: ev.Delta}), nil

	case evOutputItemDone:
		return d.itemDone(ev), nil

	case evOutputTextDone:
		// 完整终态在这一帧。此前整个事件落到 default 被丢掉，只发终态
		// 不发增量的 done-only 上游整段正文一个字都到不了客户端。
		return d.backfill(partKey(ev.OutputIndex, ev.ContentIndex), ir.BlockText,
			ev.Text, ir.EvTextDelta), nil

	case evRefusalDone:
		// 终态帧携带完整拒绝正文：done-only 上游全靠这一帧。块型与
		// delta 路径一致（refusal 独立块），backfill 只补缺失后缀。
		return d.backfill(partKey(ev.OutputIndex, ev.ContentIndex), ir.BlockRefusal,
			ev.Refusal, ir.EvTextDelta), nil

	case evContentPartDone:
		// part 级终态同样可能带着完整正文：网关漏发 output_text.done 时
		// 它是唯一来源。块闭合仍统一在 output_item.done 做，
		// 在这里也闭合会产出重复的 block_stop。
		if ev.Part == nil {
			return nil, nil
		}
		switch ev.Part.Type {
		case partOutputText, partRefusal, "":
			kind := ir.BlockText
			if ev.Part.Type == partRefusal {
				kind = ir.BlockRefusal
			}
			out := d.backfill(partKey(ev.OutputIndex, ev.ContentIndex), kind,
				partText(ev.Part), ir.EvTextDelta)
			return append(out, d.doneCitations(
				partKey(ev.OutputIndex, ev.ContentIndex), ev.Part)...), nil
		}
		return nil, nil

	case evReasoningSummaryTextDone, evReasoningTextDone:
		// 不在这里关块：encrypted_content 要到 output_item.done 才给，
		// 提前关会丢签名（判据同 donesignature_test）。
		return d.backfill(reasoningKey(ev.OutputIndex), ir.BlockThinking,
			ev.Text, ir.EvThinkingDelta), nil

	case evReasoningSummaryPartDone:
		// 有的网关只发这一帧推理终态。
		if ev.Part == nil {
			return nil, nil
		}
		return d.backfill(reasoningKey(ev.OutputIndex), ir.BlockThinking,
			ev.Part.Text, ir.EvThinkingDelta), nil

	case evCompleted, evIncomplete, evFailed:
		return d.complete(ev), nil

	case evError:
		// 错误是终止性的：收流，后续帧不再产生正常收尾（否则 completed
		// 会把已经报错的流伪装成 StopEndTurn 正常结束）。
		// 但不检查 d.done：错误详情可能迟于收尾帧到达（上游实现不一），
		// 那时仍要把错误交出去。重复的错误帧用 sawError 去重。
		d.done = true
		if d.sawError {
			return nil, nil
		}
		d.sawError = true
		return []ir.Event{{Type: ir.EvError,
			Err: streamError(ev, "upstream stream error")}}, nil

	case evReasoningSummaryPartAdded:
		// 推理摘要 part 边界标记：正文随 reasoning_summary_part.done 回补，
		// 本帧没有要转的内容。它是官方事件，计 unknown 会把正常形状报成
		// 上游乱发，归进度帧账。
		d.droppedProgress++
		return nil, nil

	default:
		// response.queued 与托管调用的进度事件（*_call.* 的
		// in_progress/searching/completed、partial_image、mcp_call_arguments
		// 与 code_interpreter_call_code 的增量）没有 IR 事件对应物：终态
		// 内容随 done 帧完整到达，进度帧只是过程信号。忽略但分类计数——
		// 同族转发丢帧与遇到本仓不认识的新事件型，都经 Notes() 报出。
		if isProgressEvent(kind) {
			d.droppedProgress++
		} else {
			d.droppedUnknown++
		}
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
		out.Created = ev.Response.CreatedAt
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
				ID:     ev.Item.CallID,
				Name:   ev.Item.Name,
				ItemID: ev.Item.ID,
			}},
		})
		// 有实现在开启帧就给出完整 arguments 且不再发增量，当一次 delta 发出。
		if ev.Item.Arguments != "" {
			d.accText(idx, ev.Item.Arguments)
			out = append(out, ir.Event{Type: ir.EvToolInput, Index: idx, Text: ev.Item.Arguments})
		}
		return out, nil
	case itemCustomToolCall:
		// 自定义工具调用条目：入参是自由文本，块要带 Kind 开启，
		// 聚合器才知道终态该落 InputText 而不是 JSON 参数槽。
		d.customKinds[ev.OutputIndex] = true
		key := callKey(ev.OutputIndex)
		if _, exists := d.lookup(key); exists {
			return out, nil
		}
		idx := d.allocate(key)
		out = append(out, ir.Event{
			Type:  ir.EvBlockStart,
			Index: idx,
			Block: &ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
				ID:     ev.Item.CallID,
				Name:   ev.Item.Name,
				Kind:   ir.ToolCustom,
				ItemID: ev.Item.ID,
			}},
		})
		// 开启帧直接带全量 input 的实现同 function_call：当一次 delta 发出。
		if ev.Item.Input != "" {
			d.accText(idx, ev.Item.Input)
			out = append(out, ir.Event{Type: ir.EvToolInput, Index: idx, Text: ev.Item.Input})
		}
		return out, nil
	case itemReasoning:
		key := reasoningKey(ev.OutputIndex)
		if _, exists := d.lookup(key); exists {
			return out, nil
		}
		idx := d.allocate(key)
		block := ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{SignatureFrom: Name, ItemID: ev.Item.ID}}
		if ev.Item.EncryptedContent != "" {
			block.Thinking.Signature = ev.Item.EncryptedContent
			d.rememberSig(key, ev.Item.EncryptedContent)
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
	reasoning := reasoningKey(ev.OutputIndex)
	var out []ir.Event
	if ev.Item != nil {
		switch ev.Item.Type {
		case itemMessage:
			// done-only 上游的整条正文只在 item.content 里，前面一帧增量都没有。
			out = append(out, d.completeItemParts(ev.OutputIndex, ev.Item.Content)...)
		case itemFunctionCall:
			// 完整参数可能只在 item.done 里：只发终态的调用一帧增量都没有。
			out = append(out, d.backfill(callKey(ev.OutputIndex), ir.BlockToolUse,
				ev.Item.Arguments, ir.EvToolInput)...)
		case itemCustomToolCall:
			// done-only 上游的自由文本入参只在 item.input 里。
			d.customKinds[ev.OutputIndex] = true
			// 先按 custom 形态补开块再回补：backfill 自己开块时不知道形态，
			// 会把自由文本落进 JSON 参数槽。
			_, opened := d.slotCall(ev.OutputIndex)
			out = append(out, opened...)
			out = append(out, d.backfill(callKey(ev.OutputIndex), ir.BlockToolUse,
				ev.Item.Input, ir.EvToolInput)...)
		case itemReasoning:
			// 摘要快照只在 item.done 里：有的网关不发任何 reasoning_* 终止帧。
			// 回补必须排在下面的签名增量之前，思考正文才不会落到签名后面。
			// summary 为空时正文在 content 数组（reasoning_text），双路径兜底。
			text := joinSummary(ev.Item.Summary)
			if text == "" {
				text = decodeReasoningContent(ev.Item.Content)
			}
			out = append(out, d.backfill(reasoning, ir.BlockThinking,
				text, ir.EvThinkingDelta)...)
		default:
			// 托管输出项（web_search_call / file_search_call 等）与未来的
			// 新条目类型：本变换没有映射，整项不可见。在 done 帧计数（每个
			// 条目恒有一次 done，added 可能缺席），Notes() 收尾时报出——
			// 静默丢掉会让客户端对不上「付了托管执行的钱却看不到产出」。
			if ev.Item.Type != "" {
				d.hostedItems++
			}
		}
	}
	for _, s := range d.open {
		if !strings.HasPrefix(s.key, prefix) {
			continue
		}
		// 签名排在闭块之前：之后到的签名增量聚合器不收（那个块已经不在
		// open 列里），会静默丢掉。
		if s.key == reasoning && ev.Item.EncryptedContent != "" {
			switch {
			case s.sig == "":
				out = append(out, ir.Event{Type: ir.EvSigDelta, Index: s.index,
					Text: ev.Item.EncryptedContent, SignatureFrom: Name})
			case s.sig != ev.Item.EncryptedContent:
				// 两帧给了不同的密文。不发第二条（会拼成垃圾）也不静默取一份：
				// 这种形态说明上游行为超出预期，运维得看见它。
				d.notes = append(d.notes,
					"upstream gave two different reasoning signatures for "+
						"the same item; kept the first one")
			}
		}
		out = append(out, ir.Event{Type: ir.EvBlockStop, Index: s.index})
	}
	d.forget(prefix)
	return out
}

// rememberSig 记下某个块已经拿到的签名，供 itemDone 判是否需要补发。
func (d *streamDecoder) rememberSig(key, sig string) {
	for i := range d.open {
		if d.open[i].key == key {
			d.open[i].sig = sig
			return
		}
	}
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
	// failed 帧恒交错误再收束流：错误详情可能已在裸 error 帧交出去
	// （sawError 去重），但 failed 不带任何错误体时也不能让流伪装成
	// 正常结束——至少给一个兜底错误。
	if ev.Type == evFailed && !d.sawError {
		d.sawError = true
		out = append(out, ir.Event{
			Type: ir.EvError,
			Err:  streamError(ev, "upstream response failed"),
		})
	}
	return append(out, delta, ir.Event{Type: ir.EvMessageStop})
}

func (d *streamDecoder) closeAll() []ir.Event {
	out := make([]ir.Event, 0, len(d.open))
	for _, s := range d.open {
		out = append(out, ir.Event{Type: ir.EvBlockStop, Index: s.index})
		d.closed[s.key] = struct{}{}
	}
	d.open = nil
	return out
}

// backfill 用 part 级 done 帧携带的完整终态补齐尚未发出的后缀，块尚未开时补开。
// done 帧带的是完整值而不是新一份内容：只发终态不发增量的 done-only 上游，
// 整段正文只在这类帧里出现，整帧丢掉就一个字都到不了客户端。
// 块已关就不动：再发增量会被追加到已定稿的条目上（对齐 new-api
// mergeFinalValue 的 block.Stopped 判据）。
func (d *streamDecoder) backfill(key string, kind ir.BlockType, full string, typ ir.EventType) []ir.Event {
	if full == "" {
		return nil
	}
	if _, closed := d.closed[key]; closed {
		return nil
	}
	idx, opened := d.slot(key, kind)
	suffix, ok := missingSuffix(d.delivered(idx), full)
	if !ok {
		return opened
	}
	d.accText(idx, suffix)
	return append(opened, ir.Event{Type: typ, Index: idx, Text: suffix})
}

// completeItemParts 从 output_item.done 的 message content 回补正文：
// done-only 上游的整条正文只在这里出现。part 在数组里的位置就是它的
// content_index，与流式 part 帧的寻址一致。
func (d *streamDecoder) completeItemParts(oi int, raw json.RawMessage) []ir.Event {
	if len(raw) == 0 {
		return nil
	}
	var parts []wirePart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	var out []ir.Event
	for n := range parts {
		switch parts[n].Type {
		case partOutputText, partRefusal, "":
			// 逐 token 概率没有 IR 槽位：只探测计数、经 Notes() 报出。
			if len(parts[n].LogProbs) > 0 && string(parts[n].LogProbs) != "null" {
				d.droppedLogprobs++
			}
			kind := ir.BlockText
			if parts[n].Type == partRefusal {
				kind = ir.BlockRefusal
			}
			out = append(out, d.backfill(partKey(oi, n), kind,
				partText(&parts[n]), ir.EvTextDelta)...)
			out = append(out, d.doneCitations(partKey(oi, n), &parts[n])...)
		}
	}
	return out
}

// doneCitations 从 done 帧的 part 快照回补来源标注：漏发 annotation.added
// 增量帧的网关，标注只在终态里出现。只挂到已存在的正文槽位（backfill
// 可能刚补开）；没有槽位说明该 part 没有正文，标注无处依附。
// 与增量帧的重叠由聚合器的 DedupeCitations 兜住，重复回补无害。
func (d *streamDecoder) doneCitations(key string, p *wirePart) []ir.Event {
	if p == nil {
		return nil
	}
	cs := decodeAnnotations(p.Annotations)
	if len(cs) == 0 {
		return nil
	}
	idx, ok := d.lookup(key)
	if !ok {
		return nil
	}
	return []ir.Event{{Type: ir.EvCitation, Index: idx, Citations: cs}}
}

// missingSuffix 判 done 帧携带的完整终态与已发出前缀的关系：终态是已发内容的
// 延长才补缺失后缀；一致（增量已给全）或分叉（上游自相矛盾）都不补——分叉时
// 保留已下发的增量，不追加成畸形正文。new-api mergeFinalValue 与 cc-switch
// missing_suffix 用的是同一套判据。
func missingSuffix(delivered, full string) (string, bool) {
	if !strings.HasPrefix(full, delivered) || full == delivered {
		return "", false
	}
	return full[len(delivered):], true
}

// accText 把刚发出的增量记进槽位账，供 done 帧回补时判前缀。
func (d *streamDecoder) accText(index int, delta string) {
	for i := range d.open {
		if d.open[i].index == index {
			d.open[i].text += delta
			return
		}
	}
}

// delivered 读出该块已发出的内容前缀。
func (d *streamDecoder) delivered(index int) string {
	for _, s := range d.open {
		if s.index == index {
			return s.text
		}
	}
	return ""
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

// slotCall 是调用条目的 slot：块开启可能发生在增量帧上（上游漏发
// output_item.added），此时要按 customKinds 把形态带上，聚合器才知道
// 累积的入参是自由文本而不是 JSON 参数。
func (d *streamDecoder) slotCall(output int) (int, []ir.Event) {
	key := callKey(output)
	if idx, ok := d.lookup(key); ok {
		return idx, nil
	}
	idx := d.allocate(key)
	tu := &ir.ToolUse{}
	if d.customKinds[output] {
		tu.Kind = ir.ToolCustom
	}
	return idx, []ir.Event{{Type: ir.EvBlockStart, Index: idx,
		Block: &ir.Block{Type: ir.BlockToolUse, ToolUse: tu}}}
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
// 键同时记入 closed：迟到的 part 级 done 帧不得再往这些块上回补。
func (d *streamDecoder) forget(prefix string) {
	kept := d.open[:0]
	for _, s := range d.open {
		if strings.HasPrefix(s.key, prefix) {
			d.closed[s.key] = struct{}{}
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
	resp, _, err := DecodeResponseLossy(body)
	return resp, err
}

// DecodeResponseLossy 实现 codec.LossyResponseDecoder：除响应外交出解码
// 损耗说明（当前只有一类——没有映射的托管输出项）。
func DecodeResponseLossy(body []byte) (*ir.Response, []string, error) {
	var w wireResponse
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, nil, ir.NewError(ir.ErrUpstream, 0, "",
			fmt.Sprintf("undecodable response: %v", err))
	}
	// 终态失败的响应不得伪造成 completed：failed/cancelled 的 output 往往
	// 是空的，错误只在 error 字段上，照解会产出一份 200 空「成功」，
	// 与流式路径遇到 failed 帧的口径相反。错误归类与 decodeErrorBody 同源。
	switch w.Status {
	case "failed", "cancelled":
		if w.Error == nil {
			return nil, nil, ir.NewError(ir.ErrUpstream, 0, "",
				"upstream response "+w.Status)
		}
		return nil, nil, convertError(0, w.Error)
	}
	// queued/in_progress 等非终态：上游还没生成完。解下去会得到一份
	// 内容残缺的「正常」响应，按可重试的上游错误处理，让调用方换目标
	// 或稍后重试。
	switch w.Status {
	case "", "completed", "incomplete":
	default:
		return nil, nil, ir.NewError(ir.ErrUpstream, 0, "",
			"upstream returned non-terminal status "+w.Status)
	}
	out := &ir.Response{
		ID:          w.ID,
		Model:       w.Model,
		StopReason:  stopReasonFor(&w),
		Content:     []ir.Block{},
		ServiceTier: w.ServiceTier,
		Created:     w.CreatedAt,
	}
	if w.Usage != nil {
		out.Usage = convertUsage(*w.Usage)
	}
	hosted := 0
	logprobs := 0
	for _, item := range w.Output {
		switch item.Type {
		case itemMessage:
			// 逐 token 概率没有 IR 槽位：只探测计数、经注记报出，
			// 判据与流式 completeItemParts 相同。
			logprobs += countLogprobsParts(item.Content)
			blocks, err := decodeContent(item.Content)
			if err != nil {
				return nil, nil, ir.NewError(ir.ErrUpstream, 0, "",
					fmt.Sprintf("undecodable output content: %v", err))
			}
			out.Content = append(out.Content, blocks...)
		case itemFunctionCall:
			out.Content = append(out.Content, ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
				ID:     item.CallID,
				Name:   item.Name,
				Input:  item.Arguments,
				ItemID: item.ID,
			}})
		case itemCustomToolCall:
			// 自由文本入参原文进 InputText，Input 放 {"input":…} 投影，
			// 口径与请求侧 appendItem 相同。
			out.Content = append(out.Content, ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
				ID:        item.CallID,
				Name:      item.Name,
				Kind:      ir.ToolCustom,
				InputText: item.Input,
				Input:     string(ir.MarshalCustomInput(item.Input)),
				ItemID:    item.ID,
			}})
		case itemReasoning:
			text := joinSummary(item.Summary)
			if text == "" {
				// summary 为空时正文在 content 数组（reasoning_text）：
				// 双路径兜底，不读整条思考正文静默丢掉。
				text = decodeReasoningContent(item.Content)
			}
			out.Content = append(out.Content, ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{
				Text:          text,
				Signature:     item.EncryptedContent,
				SignatureFrom: Name,
				ItemID:        item.ID,
			}})
		default:
			// 托管输出项与未来的新条目类型：没有映射，整项丢弃并计数，
			// 口径与流式 itemDone 的 default 相同。
			if item.Type != "" {
				hosted++
			}
		}
	}
	var notes []string
	if hosted > 0 {
		notes = append(notes, codec.HostedOutputItemsNote(hosted))
	}
	if logprobs > 0 {
		notes = append(notes, codec.LogProbsDropNote(logprobs))
	}
	return out, notes, nil
}

// countLogprobsParts 数 content 数组里带 logprobs 载荷的 part 数（逐 token
// 概率没有 IR 槽位，只探测计数）。解析失败按零计：内容解码由调用方负责，
// 这里不因同一份原文报两次错。
func countLogprobsParts(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var parts []wirePart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return 0
	}
	n := 0
	for _, p := range parts {
		if len(p.LogProbs) > 0 && string(p.LogProbs) != "null" {
			n++
		}
	}
	return n
}

func DecodeError(status int, header http.Header, body []byte) *ir.Error {
	return codec.WithRetryAfter(decodeErrorBody(status, body), header)
}

func decodeErrorBody(status int, body []byte) *ir.Error {
	// 字段类型不匹配不算「什么都没解到」：外层 body 已是合法 JSON，Go 的
	// 解码器记下类型错误后仍会解完其余键——message 写成数字时，解得好的
	// code/type 不能跟着一起丢，归因靠的是它们。
	var env wireErrorEnvelope
	_ = json.Unmarshal(body, &env)
	if env.Error.Message != "" {
		return codec.WithParam(convertError(status, &env.Error), body)
	}
	// 有些错误是裸的 response 对象，错误挂在 error 字段上。
	var resp wireResponse
	_ = json.Unmarshal(body, &resp)
	if resp.Error != nil && resp.Error.Message != "" {
		return codec.WithParam(convertError(status, resp.Error), body)
	}
	// 两种规范形状都没解出消息：可能是 message 位上的类型不匹配，
	// 其余键已经解完——code 救回来，消息回落原文。
	code := env.Error.Code
	if code == "" {
		code = env.Error.Type
	}
	if code == "" && resp.Error != nil {
		code = resp.Error.Code
		if code == "" {
			code = resp.Error.Type
		}
	}
	if code != "" {
		return codec.WithParam(codec.SalvagedError(status, body, code), body)
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

// streamError 提取错误帧的错误体，三层回落：官方裸 error 帧的顶层
// error 对象 → failed 帧的 response.error → 部分网关平铺到帧顶层的
// code/message/param 三键。三层都空时用 fallbackMsg 兜底，
// 不让客户端拿到一个无消息的错误。
func streamError(ev wireStreamEvent, fallbackMsg string) *ir.Error {
	var b wireError
	switch {
	case ev.Error != nil:
		b = *ev.Error
	case ev.Response != nil && ev.Response.Error != nil:
		b = *ev.Response.Error
	default:
		b = wireError{Code: ev.Code, Message: ev.Message, Param: ev.Param}
	}
	if b.Message == "" {
		b.Message = fallbackMsg
	}
	return convertError(0, &b)
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
		// custom_tool_call 同样是一次待执行的调用：漏判会让客户端
		// 以为回答正常结束而不去执行工具。
		if item.Type == itemFunctionCall || item.Type == itemCustomToolCall {
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
