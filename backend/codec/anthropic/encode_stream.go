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
	// blockOrder 让 Finish 按开启顺序闭合，避免 map 遍历顺序不定
	// 导致同样的输入产出不同的帧序。
	blockOrder []int
	sentDelta  bool
	// notes 是响应侧丢弃说明，累加后由 Notes 去重排序交出。
	notes []string
}

// Notes 实现 codec.StreamNotes。
func (e *streamEncoder) Notes() []string {
	notes := e.notes
	if e.badToolArgs > 0 {
		notes = append(notes, ir.RawArgsPassNote(e.badToolArgs))
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
		toolArgs: map[int][]byte{}}
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
			wb, ok, err := encodeBlock(*ev.Block)
			if err != nil {
				return nil, err
			}
			if !ok {
				return out, nil
			}
			block = wb
			// 块开启时入参尚未到齐，Anthropic 要求这里是空对象，
			// 内容由后续 input_json_delta 累积。
			if block.Type == blockToolUse {
				block.Input = json.RawMessage(`{}`)
				e.toolPending[ev.Index] = true
				e.toolArgs[ev.Index] = nil
			}
		}
		e.open(ev.Index)
		frame, err := marshalFrame(evContentBlockStart, streamEvent{
			Type: evContentBlockStart, Index: ev.Index, Block: &block,
		})
		if err != nil {
			return nil, err
		}
		return append(out, frame), nil

	case ir.EvTextDelta:
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

	case ir.EvBlockStop:
		if !e.openBlocks[ev.Index] {
			return nil, nil
		}
		return e.closeFrames(ev.Index), nil

	case ir.EvMessageDelta:
		// 档位也可能只在收尾帧到达（非流式响应投影成事件时就是这样），
		// 只在 message_start 判会漏掉那一形态。
		if ev.ServiceTier != "" {
			e.notes = append(e.notes, codec.DroppedServiceTierNote(Name))
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
	// 本协议的消息头里没有执行档位的位置。上游报了就得说一声——
	// 这一维决定计费，无声丢掉会让客户端按自己点的档位对账。
	if ev.ServiceTier != "" {
		e.notes = append(e.notes, codec.DroppedServiceTierNote(Name))
	}
	msg := &streamMsg{ID: ev.MessageID, Model: ev.Model, Role: string(ir.RoleAssistant)}
	if msg.ID == "" {
		msg.ID = "msg_unknown"
	}
	if ev.Usage != nil {
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
	out := streamEvent{
		Type: evMessageDelta,
		Delta: &streamDelta{
			StopReason: renderStopReason(ev.StopReason),
			// 走同一个采纳判据：兜底成 end_turn 的那一支不该带着序列，
			// 而兜底发生在下面几行，所以这里先按原始终止原因判。
			StopSequence: adoptStopSequence(ev.StopReason, ev.StopSequence),
		},
	}
	if out.Delta.StopReason == "" {
		out.Delta.StopReason = "end_turn"
	}
	if ev.Usage != nil {
		u := renderUsage(*ev.Usage)
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
		Usage:        renderUsage(resp.Usage),
	}
	if w.ID == "" {
		w.ID = "msg_unknown"
	}
	if w.StopReason == "" {
		w.StopReason = "end_turn"
	}
	w.Content = make([]wireBlock, 0, len(resp.Content))
	for _, b := range resp.Content {
		wb, ok, err := encodeBlock(b)
		if err != nil {
			return nil, err
		}
		if ok {
			w.Content = append(w.Content, wb)
		}
	}
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
	case ir.ErrInvalidRequest, ir.ErrContextExceeded:
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
	return wireUsage{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheReadInputTokens:     u.CacheReadTokens,
		CacheCreationInputTokens: u.CacheWriteTokens,
	}
}

func marshalFrame(event string, payload streamEvent) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return codec.EncodeFrame(event, data), nil
}
