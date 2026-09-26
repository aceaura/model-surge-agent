package anthropic

import (
	"encoding/json"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// streamEncoder 把 IR 事件编成 Anthropic SSE 帧。
//
// 它跟踪已开启的块与是否已发过 message_start，因为客户端 SDK 对帧序有要求：
// message_start 必须先到，块必须成对开闭，末尾必须有 message_stop。
// 上游流被中途掐断时，Finish 要把这些缺口补齐。
type streamEncoder struct {
	started bool
	stopped bool
	// errored 表示流已用错误帧收尾。此后既不补正常终止帧，也丢弃迟到的增量：
	// 错误之后再发内容或 message_stop，客户端会把这轮当成功而存下残缺历史。
	errored    bool
	openBlocks map[int]bool
	// toolPending 记录已开启但还没见过任何 input 增量的 tool_use 块。
	// 客户端（含官方 SDK）只从 input_json_delta 拼参数，一个 delta 都不发
	// 等于参数是空串——拼出来不是合法 JSON，关块时要补一个 "{}"。
	toolPending map[int]bool
	// toolArgs 累积各 tool_use 块已下发的入参增量。input_json_delta 一旦
	// 发出就不可改写，畸形参数（多为 max_tokens 截断）只能原样透传，
	// 关块时校验累积值并计数，由 Notes 报出——否则客户端会把一次
	// 参数损坏的调用当正常完成存进历史。
	toolArgs map[int][]byte
	// badToolArgs 是关块时判定畸形的工具调用数，Notes() 报出。
	badToolArgs int
	// droppedCitations 是反推失败被整条丢弃的引用数：跨协议投影来、无 Raw
	// 可透传又切不出 cited_text 的引用，encodeCitations 会跳过它们（带空
	// cited_text 上游 400）。丢弃不能静默，计数经 Notes() 报出。
	droppedCitations int
	// downgradedMediaImages / downgradedMediaFiles 是被降级为文本占位的模型
	// 产出附件数（图片单列、音频/文档/文件合列）。上游返回本协议白名单
	// （图片 + PDF）之外的媒体时 encodeBlock 会降级，判据与非流式响应扫描
	// 同源（mediaDowngraded），计数经 Notes() 报出。
	downgradedMediaImages int
	downgradedMediaFiles  int
	// emptyMedia 是因无任何可投递载荷而被 encodeBlock 整块跳过的模型产出
	// 媒体数（与降级互斥：降级有载荷、空壳没有）。判据 mediaEmptyShell 与
	// 非流式 countResponseEmptyMedia 同源，计数经 Notes() 报出。
	emptyMedia int
	// blockOrder 让 Finish 按开启顺序闭合，避免 map 遍历顺序不定
	// 导致同样的输入产出不同的帧序。
	blockOrder []int
	// text 累积各块已下发的正文。跨协议投影来的引用没有 Raw，编成
	// web_search_result_location 时 cited_text 只能在正文上按范围反推，
	// 而引用总在正文之后到达，所以必须逐块累积。
	text      map[int]string
	sentDelta bool
	// tierSent 档位回显已随 message_start 下发。
	tierSent bool
	// droppedTier 没能下发的档位回显原值：越集、或到得太晚
	// （message_delta 没有 service_tier 槽位，上游只在收尾帧报的
	// 回显送不出去）。Notes() 收尾时报出。
	droppedTier string
	// notes 是响应侧丢弃说明，累加后由 Notes 去重排序交出。
	notes []string
	// usage 累计本流交付过的用量：只服务 Notes() 的细分损耗判据
	//（音频/预测四位是 chat 专属维度，本协议 usage 没有槽位），
	// 帧渲染仍按事件原值下发，不读这份累计。
	usage ir.Usage
}

// Notes 实现 codec.StreamNotes。
func (e *streamEncoder) Notes() []string {
	notes := e.notes
	if e.badToolArgs > 0 {
		notes = append(notes, ir.RawArgsPassNote(e.badToolArgs))
	}
	if e.droppedCitations > 0 {
		notes = append(notes, codec.CitationResolveDropNote(e.droppedCitations))
		e.droppedCitations = 0
	}
	if e.downgradedMediaImages+e.downgradedMediaFiles > 0 {
		notes = append(notes, codec.MediaOutputDropNote(e.downgradedMediaImages, e.downgradedMediaFiles))
		e.downgradedMediaImages, e.downgradedMediaFiles = 0, 0
	}
	if e.emptyMedia > 0 {
		notes = append(notes, codec.EmptyMediaOutputDropNote(e.emptyMedia))
		e.emptyMedia = 0
	}
	if e.droppedTier != "" {
		notes = append(notes, codec.TierEchoDropNote(e.droppedTier))
		e.droppedTier = ""
	}
	// usage 细分维度：chat 专属的音频/预测四位本协议没有槽位，聚合
	// 总量不丢，细分蒸发要报出，判据与非流式 EncodeResponseLossy 同源。
	if dims := codec.UsageDropDims(&e.usage, Name); len(dims) > 0 {
		notes = append(notes, codec.UsageDetailDropNote(dims))
		e.usage = ir.Usage{}
	}
	return codec.DedupeNotes(notes)
}

// HeartbeatFrame 实现 codec.StreamHeartbeat：本协议有自己的 ping 事件类型。
//
// 用 ping 而不是 SSE 注释：Anthropic 的客户端 SDK 按事件类型分派，
// ping 是它已知且会忽略的一类；注释帧虽然规范上也该被忽略，
// 但那是对解析器的要求，而按类型分派的实现可能压根没走到注释分支。
//
// 不置 started/stopped 也不动块状态：保活帧不参与帧序不变式，
// 它在 message_start 之前之后都合法。
//
// marshal 失败返回 nil：调用方对 nil 的处置是不发，而保活帧漏一次无后果——
// 下一个周期还会再来。
func (e *streamEncoder) HeartbeatFrame() []byte {
	frame, err := marshalFrame(evPing, streamEvent{Type: evPing})
	if err != nil {
		return nil
	}
	return frame
}

func newStreamEncoder() *streamEncoder {
	return &streamEncoder{openBlocks: map[int]bool{}, toolPending: map[int]bool{},
		toolArgs: map[int][]byte{}, text: map[int]string{}}
}

func (e *streamEncoder) Encode(ev ir.Event) ([][]byte, error) {
	if e.errored {
		return nil, nil
	}
	switch ev.Type {
	case ir.EvMessageStart:
		return e.encodeStart(ev)

	case ir.EvBlockStart:
		// 上游可能不发 message_start（或本服务先收到块），
		// 补一个，否则客户端 SDK 会因缺少消息头而报错。
		out := e.ensureStarted(ev)
		block := wireBlock{Type: blockText}
		if ev.Block != nil {
			// 本协议白名单（图片 + PDF）之外的模型产出附件会被 encodeBlock
			// 降级为文本占位，计数经 Notes() 报出，判据与非流式响应扫描同源。
			if mediaDowngraded(*ev.Block) {
				if ev.Block.Type == ir.BlockImage {
					e.downgradedMediaImages++
				} else {
					e.downgradedMediaFiles++
				}
			}
			// 空壳媒体（无载荷）走整块跳过、不是降级，mediaDowngraded 数不到，
			// 单列计数。与非流式 countResponseEmptyMedia 判据同源。
			if mediaEmptyShell(*ev.Block) {
				e.emptyMedia++
			}
			wb, ok, err := encodeBlock(*ev.Block)
			if err != nil {
				return nil, err
			}
			if !ok {
				// 空壳跳过（见 encode_request.go 的 BlockThinking 分支）服务的是
				// 请求/响应体完整性；流式开块是它的唯一例外：thinking 块的起始
				// 快照必然正文为空、签名未到（增量随后才来），照跳会让后续
				// thinking_delta 无块可落。官方起始帧本就是空壳形状
				// {"type":"thinking"}，这里按原样开块。
				if ev.Block.Type == ir.BlockThinking && ev.Block.Thinking != nil &&
					!ev.Block.Thinking.Redacted {
					block = wireBlock{Type: blockThinking}
				} else {
					return out, nil
				}
			} else {
				block = wb
			}
			if ev.Block != nil {
				// 块开启时入参尚未到齐，Anthropic 要求这里是空对象，
				// 内容由后续 input_json_delta 累积。server_tool_use 的查询串
				// 走同一条通道，同款处置。
				if block.Type == blockToolUse || block.Type == blockServerToolUse {
					block.Input = json.RawMessage(`{}`)
					e.toolPending[ev.Index] = true
					e.toolArgs[ev.Index] = nil
				}
			}
		}
		e.open(ev.Index)
		// content_block 以原文槽位写出：不透明块的 wireBlock.Raw 要整块逐字带回，
		// 先 marshal 成 BlockRaw 再进帧。streamEvent 的 content_block 槽位解码侧
		// 也用它收原文（编解码共用一个 RawMessage 字段，见 wire.go），普通块经
		// wireBlock.MarshalJSON 仍按字段序列化，产物与改动前一致。
		rawBlock, err := json.Marshal(block)
		if err != nil {
			return nil, err
		}
		frame, err := marshalFrame(evContentBlockStart, streamEvent{
			Type: evContentBlockStart, Index: ev.Index, BlockRaw: rawBlock,
		})
		if err != nil {
			return nil, err
		}
		return append(out, frame), nil

	case ir.EvTextDelta:
		// 先累积再编帧：随后的 citations_delta 要用这份文本回推偏移量。
		e.text[ev.Index] += ev.Text
		return e.encodeDelta(ev, &streamDelta{Type: deltaText, Text: ev.Text})
	case ir.EvThinkingDelta:
		return e.encodeDelta(ev, &streamDelta{Type: deltaThinking, Thinking: ev.Text})
	case ir.EvSigDelta:
		if note, drop := codec.ForeignEventSignature(ev.Text, ev.SignatureFrom, Name); drop {
			// 丢增量而不是丢整个推理块：文本部分对客户端仍然有用，
			// 只有签名是它验不了、下一轮会被上游拒收的那部分。
			e.notes = append(e.notes, note)
			return nil, nil
		}
		return e.encodeDelta(ev, &streamDelta{Type: deltaSignature, Signature: ev.Text})
	case ir.EvToolInput:
		e.toolArgs[ev.Index] = append(e.toolArgs[ev.Index], ev.Text...)
		out, err := e.encodeDelta(ev, &streamDelta{Type: deltaInputJSON, PartialJSON: ev.Text})
		// encodeDelta 可能刚自动开启这个块（置位 pending），所以清标记
		// 必须放在它之后：见过真实增量的块不再是零增量。
		delete(e.toolPending, ev.Index)
		return out, err

	case ir.EvCitation:
		// 不走 encodeDelta 的自动开块：引用没有正文可发，为它单开一个块
		// 会让客户端多出一个空文本块。块没开就直接丢——上游没给过这个
		// index 的任何帧，凭空补开等于伪造块结构。
		if !e.openBlocks[ev.Index] {
			return nil, nil
		}
		// 逐条编帧：本协议的 citations_delta 一帧只带一条引用。
		// 带 Raw 的原样转出，投影来的重建；cited_text 反推不出的条目
		// 由 encodeCitations 整条丢弃，这里同步计数，Notes() 收尾报出。
		e.droppedCitations += countUnresolvableCitations(e.text[ev.Index], ev.Citations)
		var out [][]byte
		for _, raw := range encodeCitations(e.text[ev.Index], ev.Citations) {
			frame, err := marshalFrame(evContentBlockDelta, streamEvent{
				Type:  evContentBlockDelta,
				Index: ev.Index,
				Delta: &streamDelta{Type: deltaCitations, Citation: raw},
			})
			if err != nil {
				return nil, err
			}
			out = append(out, frame)
		}
		return out, nil

	case ir.EvBlockStop:
		if !e.openBlocks[ev.Index] {
			return nil, nil
		}
		return e.closeFrames(ev.Index), nil

	case ir.EvMessageDelta:
		// 档位也可能只在收尾帧到达（chat 系上游的后续 chunk 才带、
		// 非流式响应投影成事件时就是这样）。message_delta 没有
		// service_tier 槽位：message_start 已经带过就算了，没带过
		// 即便值集装得下也送不出去，照实报出。
		if ev.ServiceTier != "" && !e.tierSent && e.droppedTier == "" {
			e.droppedTier = ev.ServiceTier
		}
		out := e.ensureStarted(ev)
		// stop_reason 要在所有块闭合之后才发。
		out = append(out, e.closeAll()...)
		frame, err := e.messageDelta(ev)
		if err != nil {
			return nil, err
		}
		e.sentDelta = true
		return append(out, frame), nil

	case ir.EvMessageStop:
		if e.stopped {
			return nil, nil
		}
		out := e.ensureStarted(ev)
		out = append(out, e.closeAll()...)
		if !e.sentDelta {
			// 客户端要从 message_delta 拿 stop_reason 与 usage；
			// 上游没发就补一个空的，否则 SDK 拿不到终止原因。
			frame, err := e.messageDelta(ir.Event{StopReason: ir.StopEndTurn})
			if err != nil {
				return nil, err
			}
			out = append(out, frame)
			e.sentDelta = true
		}
		e.stopped = true
		frame, err := marshalFrame(evMessageStop, streamEvent{Type: evMessageStop})
		if err != nil {
			return nil, err
		}
		return append(out, frame), nil

	case ir.EvPing:
		frame, err := marshalFrame(evPing, streamEvent{Type: evPing})
		if err != nil {
			return nil, err
		}
		return [][]byte{frame}, nil

	case ir.EvError:
		// 先闭合已开的块，再发错误帧。
		//
		// 闭合块与「宣告这轮正常结束」是两件事：前者让客户端 SDK 的块状态机
		// 收束，后者才会让它以为这轮成功。只做前者——errored 置位后 Finish()
		// 仍然什么都不补，message_delta 与 message_stop 一帧都不会出现。
		out := e.closeAll()
		e.errored = true
		return append(out, RenderStreamError(ev.Err)...), nil

	default:
		return nil, nil
	}
}

func (e *streamEncoder) encodeStart(ev ir.Event) ([][]byte, error) {
	if e.started {
		return nil, nil
	}
	e.started = true
	// 官方 message_start 的 message 对象是完整 Message 形状：type/content/
	// stop_reason/stop_sequence 四键恒在（content 空数组、两个 stop 键显式
	// null），严格 SDK 按必填字段反序列化，缺键会在第一帧就解析失败。
	msg := &streamMsg{
		Type: "message", ID: ev.MessageID, Model: ev.Model, Role: string(ir.RoleAssistant),
		Content: []json.RawMessage{},
	}
	// 实际执行档位回显：同族原值下发，跨族按回显值集翻译；装不下的
	// 丢弃，Notes() 报出——这一维决定计费，无声丢掉会让客户端按自己
	// 点的档位对账。
	if ev.ServiceTier != "" {
		if tier, ok := codec.MapServiceTierEcho(ev.ServiceTier, Name); ok {
			msg.ServiceTier = tier
			e.tierSent = true
		} else {
			e.droppedTier = ev.ServiceTier
		}
	}
	if msg.ID == "" {
		msg.ID = "msg_unknown"
	}
	// container 是本家维度，直接下发。
	msg.Container = encodeContainerInfo(ev.Container)
	// 模型音频输出（chat 非流式投影而来）没有本协议槽位：丢弃并报出。
	if ev.Audio != nil {
		e.notes = append(e.notes, codec.AudioOutputDropNote())
	}
	if ev.Usage != nil {
		ir.MergeUsage(&e.usage, *ev.Usage)
		msg.Usage = renderUsage(*ev.Usage)
	}
	frame, err := marshalFrame(evMessageStart, streamEvent{Type: evMessageStart, Message: msg})
	if err != nil {
		return nil, err
	}
	return [][]byte{frame}, nil
}

// ensureStarted 在 message_start 缺失时补发一帧。
func (e *streamEncoder) ensureStarted(ev ir.Event) [][]byte {
	if e.started {
		return nil
	}
	out, err := e.encodeStart(ir.Event{Type: ir.EvMessageStart, MessageID: ev.MessageID, Model: ev.Model})
	if err != nil {
		return nil
	}
	return out
}

// encodeDelta 发一个块内增量；块未开启时先补 content_block_start，
// 否则客户端会收到指向不存在块的 delta。
func (e *streamEncoder) encodeDelta(ev ir.Event, delta *streamDelta) ([][]byte, error) {
	var out [][]byte
	if !e.openBlocks[ev.Index] {
		opened, err := e.Encode(ir.Event{
			Type:  ir.EvBlockStart,
			Index: ev.Index,
			Block: &ir.Block{Type: blockTypeForDelta(ev.Type)},
		})
		if err != nil {
			return nil, err
		}
		out = append(out, opened...)
	}
	frame, err := marshalFrame(evContentBlockDelta, streamEvent{
		Type: evContentBlockDelta, Index: ev.Index, Delta: delta,
	})
	if err != nil {
		return nil, err
	}
	return append(out, frame), nil
}

func blockTypeForDelta(t ir.EventType) ir.BlockType {
	switch t {
	case ir.EvThinkingDelta, ir.EvSigDelta:
		return ir.BlockThinking
	case ir.EvToolInput:
		return ir.BlockToolUse
	default:
		return ir.BlockText
	}
}

func (e *streamEncoder) messageDelta(ev ir.Event) ([]byte, error) {
	out := messageDeltaEvent{
		Type: evMessageDelta,
		Delta: &streamDelta{
			StopReason: renderStopReason(ev.StopReason),
			// 走同一个采纳判据：兜底成 end_turn 的那一支不该带着序列，
			// 而兜底发生在下面几行，所以这里先按原始终止原因判。
			StopSequence: adoptStopSequence(ev.StopReason, ev.StopSequence),
			Container:    encodeContainerInfo(ev.Container),
			StopDetails:  encodeStopDetails(ev.StopDetails),
		},
	}
	if out.Delta.StopReason == "" {
		out.Delta.StopReason = "end_turn"
	}
	if ev.Usage != nil {
		ir.MergeUsage(&e.usage, *ev.Usage)
		u := renderDeltaUsage(*ev.Usage)
		out.Usage = &u
	}
	return marshalFrame(evMessageDelta, out)
}

func (e *streamEncoder) open(index int) {
	if !e.openBlocks[index] {
		e.blockOrder = append(e.blockOrder, index)
	}
	e.openBlocks[index] = true
}

func (e *streamEncoder) close(index int) {
	delete(e.openBlocks, index)
}

// closeAll 按开启顺序闭合仍开着的块。
func (e *streamEncoder) closeAll() [][]byte {
	var out [][]byte
	for _, idx := range e.blockOrder {
		if !e.openBlocks[idx] {
			continue
		}
		out = append(out, e.closeFrames(idx)...)
	}
	return out
}

// closeFrames 闭合一个块。零增量的 tool_use 块先补一个 "{}" delta：
// 客户端只从 input_json_delta 拼参数，没补的话拼出空串而非合法 JSON
// （sub2api 同款：clients assemble tool input exclusively from deltas）。
func (e *streamEncoder) closeFrames(index int) [][]byte {
	var out [][]byte
	e.finishToolArgs(index)
	if e.toolPending[index] {
		delete(e.toolPending, index)
		if frame, err := marshalFrame(evContentBlockDelta, streamEvent{
			Type:  evContentBlockDelta,
			Index: index,
			Delta: &streamDelta{Type: deltaInputJSON, PartialJSON: "{}"},
		}); err == nil {
			out = append(out, frame)
		}
	}
	e.close(index)
	frame, err := marshalFrame(evContentBlockStop, streamEvent{
		Type: evContentBlockStop, Index: index,
	})
	if err != nil {
		return out
	}
	return append(out, frame)
}

// finishToolArgs 在关块时校验累积的入参。增量已发出、改写不了，
// 畸形的只能计数报出（RawArgsPassNote），让客户端知道这次调用的参数
// 不能安全执行，而不是看起来以空对象正常完成。
func (e *streamEncoder) finishToolArgs(index int) {
	raw, ok := e.toolArgs[index]
	if !ok {
		return
	}
	delete(e.toolArgs, index)
	if _, valid := ir.NormalizeToolInput(raw); !valid {
		e.badToolArgs++
	}
}

// Finish 补齐流：闭合未关的块，补 message_delta 与 message_stop。
// 上游中途断流后必须调用，否则客户端会一直等一个不会来的结束帧。
//
// 已用错误帧收尾时什么都不补：错误帧本身就是终止，再补 message_stop
// 会让客户端以为这轮正常结束。
func (e *streamEncoder) Finish() [][]byte {
	if e.stopped || e.errored {
		return nil
	}
	out, err := e.Encode(ir.Event{Type: ir.EvMessageStop})
	if err != nil {
		return nil
	}
	return out
}

// EncodeResponse 编非流式响应体。
func EncodeResponse(resp *ir.Response) ([]byte, error) {
	if resp == nil {
		return nil, nil
	}
	w := wireResponse{
		ID:           resp.ID,
		Type:         "message",
		Role:         string(ir.RoleAssistant),
		Model:        resp.Model,
		StopReason:   renderStopReason(resp.StopReason),
		StopSequence: adoptStopSequence(resp.StopReason, resp.StopSequence),
		StopDetails:  encodeStopDetails(resp.StopDetails),
		Usage:        renderUsage(resp.Usage),
		Container:    encodeContainerInfo(resp.Container),
	}
	// 实际执行档位回显：跨族按回显值集翻译，装不下的（OpenAI 系的
	// flex/scale/fast/ultrafast 等）丢弃，由 DescribeResponseTierLoss 报出。
	if tier, ok := codec.MapServiceTierEcho(resp.ServiceTier, Name); ok {
		w.ServiceTier = tier
	}
	if w.ID == "" {
		w.ID = "msg_unknown"
	}
	if w.StopReason == "" {
		w.StopReason = "end_turn"
	}
	blocks := make([]wireBlock, 0, len(resp.Content))
	for _, b := range resp.Content {
		wb, ok, err := encodeBlock(b)
		if err != nil {
			return nil, err
		}
		if ok {
			blocks = append(blocks, wb)
		}
	}
	// Content 是 RawMessage 槽位（解码侧逐块拆原文用），这里把块数组整体 marshal
	// 进去：不透明块的 wireBlock.Raw 经其 MarshalJSON 逐字嵌入，其余按字段序列化。
	content, err := json.Marshal(blocks)
	if err != nil {
		return nil, err
	}
	w.Content = content
	return json.Marshal(w)
}

// RenderError 编非流式错误响应。
func RenderError(err *ir.Error) (int, []byte) {
	status, body, _ := RenderErrorLossy(err)
	return status, body
}

// RenderErrorLossy 与 RenderError 编出同样的字节，另外报告丢掉的维度。
//
// 本协议的错误信封只有 {type,message} 两个位，上游给的 param 无处安放。
// 这条要记进 lossy：它确实丢了信息，且只在上游真的给了 param 时才出现，
// 指向性成立——与「补块闭合帧」那种恒定发生、不丢信息的动作不同。
func RenderErrorLossy(err *ir.Error) (int, []byte, []string) {
	status, env := errorEnvelope(err)
	var notes []string
	if err != nil && err.Param != "" {
		notes = append(notes, "dropped error param "+err.Param+
			" (anthropic error envelope has no param field)")
	}
	body, marshalErr := json.Marshal(env)
	if marshalErr != nil {
		return status, []byte(`{"type":"error","error":{"type":"api_error","message":"internal error"}}`), notes
	}
	return status, body, notes
}

// RenderStreamError 编流内错误帧。此时 HTTP 200 已写出，状态码不可再改，
// 错误只能作为流内的 error 事件表达。
func RenderStreamError(err *ir.Error) [][]byte {
	_, env := errorEnvelope(err)
	frame, marshalErr := marshalFrame(evError, streamEvent{Type: evError, Error: &env.Error})
	if marshalErr != nil {
		return nil
	}
	return [][]byte{frame}
}

func errorEnvelope(err *ir.Error) (int, wireErrorEnvelope) {
	if err == nil {
		err = ir.NewError(ir.ErrInternal, 500, "", "unknown error")
	}
	status := err.StatusCode
	if status < 400 {
		status = codec.StatusForKind(err.Kind)
	}
	return status, wireErrorEnvelope{
		Type:  "error",
		Error: wireError{Type: errorTypeForKind(err.Kind), Message: err.Message},
	}
}

// errorTypeForKind 用 Anthropic 的错误类型名，让客户端 SDK 能按自己的
// 分类处理，而不是看到一个陌生字符串。
func errorTypeForKind(kind ir.ErrorKind) string {
	switch kind {
	case ir.ErrInvalidRequest, ir.ErrContextExceeded, ir.ErrContentFilter:
		return "invalid_request_error"
	case ir.ErrAuth:
		return "authentication_error"
	case ir.ErrNotFound:
		return "not_found_error"
	case ir.ErrRateLimit:
		return "rate_limit_error"
	case ir.ErrUpstream, ir.ErrTimeout:
		return "api_error"
	default:
		return "api_error"
	}
}

func renderUsage(u ir.Usage) wireUsage {
	out := wireUsage{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheReadInputTokens:     u.CacheReadTokens,
		CacheCreationInputTokens: u.CacheWriteTokens,
	}
	// TTL 明细只在「已知」时写出：异族来源没这个维度，凭总量拆不出
	// 5m/1h 各占多少，伪造一个全零对象等于谎报「明细已知且都是零」。
	if u.CacheWriteDetailsKnown {
		out.CacheCreation = &wireCacheCreationUsage{
			Ephemeral5mInputTokens: u.CacheWrite5mTokens,
			Ephemeral1hInputTokens: u.CacheWrite1hTokens,
		}
	}
	// 托管工具次数与推理区域是 anthropic 专属回执：同族往返原样带回，
	// 异族来源给不出非零值，这里自然不写。
	if u.WebSearchRequests > 0 || u.WebFetchRequests > 0 {
		out.ServerToolUse = &wireServerToolUsage{
			WebSearchRequests: u.WebSearchRequests,
			WebFetchRequests:  u.WebFetchRequests,
		}
	}
	out.InferenceGeo = u.InferenceGeo
	// beta 迭代用量细分原文回写：同族往返逐字带回，异族来源给不出非空值，
	// omitempty 自然不写。
	out.Iterations = u.Iterations
	return out
}

// renderDeltaUsage 编 message_delta 的专用 usage。官方 MessageDeltaUsage
// 没有 cache_creation 对象与 inference_geo，这里不编——编了就是往帧里写
// 官方 schema 没有的键（同族非流式→流式转换必然触发：聚合 usage 带着
// 明细整体落进 EvMessageDelta）。明细与地理回显由 message_start 与
// 非流式响应承担，delta 帧不丢可送达的信息。
func renderDeltaUsage(u ir.Usage) wireMessageDeltaUsage {
	out := wireMessageDeltaUsage{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheReadInputTokens:     u.CacheReadTokens,
		CacheCreationInputTokens: u.CacheWriteTokens,
	}
	if u.WebSearchRequests > 0 || u.WebFetchRequests > 0 {
		out.ServerToolUse = &wireServerToolUsage{
			WebSearchRequests: u.WebSearchRequests,
			WebFetchRequests:  u.WebFetchRequests,
		}
	}
	// iterations 是「delta 帧不写完整 Usage 专属键」的例外：官方 beta
	// MessageDeltaUsage 与完整 usage 同形也带它，原文回写。
	out.Iterations = u.Iterations
	return out
}

// encodeStopDetails 拒绝分类回写。type 恒 "refusal"（官方该对象只在这一档
// 出现）；空字段省略——上游显式 null 与缺省语义相同，IR 不保留二者之别。
func encodeStopDetails(sd *ir.StopDetails) *wireStopDetails {
	if sd == nil {
		return nil
	}
	return &wireStopDetails{
		Type:        "refusal",
		Category:    rawStringOrNil(sd.Category),
		Explanation: rawStringOrNil(sd.Explanation),
	}
}

func rawStringOrNil(s string) json.RawMessage {
	if s == "" {
		return nil
	}
	b, _ := json.Marshal(s)
	return b
}

// marshalFrame 编一帧 SSE。payload 收 any：绝大多数帧是 streamEvent，
// message_delta 用官方的专用 usage 形状（messageDeltaEvent）。
func marshalFrame(event string, payload any) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return codec.EncodeFrame(event, data), nil
}
