package responses

import (
	"encoding/json"
	"fmt"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// streamEncoder 把 IR 事件编成 response.* 帧序列。
//
// 本协议的帧序比另外三个都严：每个条目要有 added/done 一对帧，文本条目内
// 还要再套一层 content_part 的 added/done，且终止帧必须带完整的 response
// 对象（客户端 SDK 从那里取最终结果）。所以这里要边编码边攒出输出条目，
// 到 completed 帧一次性写出。
type streamEncoder struct {
	id      string
	model   string
	stopped bool
	// errored 表示流已用 error 帧收尾。此后不再发 completed/incomplete，
	// 也丢弃迟到的增量：那个终止帧会带上残缺内容并标成 completed。
	errored bool

	// items 是已开启的条目，按 IR 块索引定位。
	items map[int]*openItem
	order []int
	// nextOutput 分配 output_index。IR 的块索引不能直接用：
	// 客户端 SDK 要求 output_index 从 0 连续递增。
	nextOutput int

	stopReason ir.StopReason
	usage      ir.Usage
}

// openItem 记录一个已开启条目的状态，用于闭合时补齐 done 帧并累积最终 response。
type openItem struct {
	outputIndex int
	kind        ir.BlockType
	// partOpen 记录 message 条目是否已发过 content_part.added。
	partOpen  bool
	text      string
	callID    string
	name      string
	args      string
	signature string
	closed    bool
}

func newStreamEncoder() *streamEncoder {
	return &streamEncoder{items: map[int]*openItem{}}
}

func (e *streamEncoder) Encode(ev ir.Event) ([][]byte, error) {
	if e.errored {
		return nil, nil
	}
	switch ev.Type {
	case ir.EvMessageStart:
		e.id = ev.MessageID
		e.model = ev.Model
		// Anthropic 上游在这一帧给 input_tokens，而本协议只在终止帧报用量，
		// 不在这里收下就永远丢了。
		if ev.Usage != nil {
			ir.MergeUsage(&e.usage, *ev.Usage)
		}
		return e.frame(evCreated, wireStreamEvent{
			Type: evCreated, Response: e.snapshot("in_progress"),
		})

	case ir.EvBlockStart:
		kind := ir.BlockText
		if ev.Block != nil {
			kind = ev.Block.Type
		}
		return e.openBlock(ev.Index, kind, ev.Block)

	case ir.EvTextDelta:
		out, item, err := e.ensureOpen(ev.Index, ir.BlockText)
		if err != nil {
			return nil, err
		}
		item.text += ev.Text
		frames, err := e.frame(evOutputTextDelta, wireStreamEvent{
			Type:        evOutputTextDelta,
			OutputIndex: item.outputIndex,
			Delta:       ev.Text,
		})
		if err != nil {
			return nil, err
		}
		return append(out, frames...), nil

	case ir.EvThinkingDelta:
		out, item, err := e.ensureOpen(ev.Index, ir.BlockThinking)
		if err != nil {
			return nil, err
		}
		item.text += ev.Text
		frames, err := e.frame(evReasoningSummaryText, wireStreamEvent{
			Type:        evReasoningSummaryText,
			OutputIndex: item.outputIndex,
			Delta:       ev.Text,
		})
		if err != nil {
			return nil, err
		}
		return append(out, frames...), nil

	case ir.EvSigDelta:
		// 本协议的 encrypted_content 只能整体给出，没有增量帧，
		// 所以攒进条目，等闭合时随 item 一起发出。
		if item, ok := e.items[ev.Index]; ok {
			item.signature += ev.Text
		}
		return nil, nil

	case ir.EvToolInput:
		out, item, err := e.ensureOpen(ev.Index, ir.BlockToolUse)
		if err != nil {
			return nil, err
		}
		item.args += ev.Text
		frames, err := e.frame(evFunctionArgsDelta, wireStreamEvent{
			Type:        evFunctionArgsDelta,
			OutputIndex: item.outputIndex,
			Delta:       ev.Text,
		})
		if err != nil {
			return nil, err
		}
		return append(out, frames...), nil

	case ir.EvBlockStop:
		return e.closeBlock(ev.Index)

	case ir.EvMessageDelta:
		if ev.StopReason != "" {
			e.stopReason = ev.StopReason
		}
		if ev.Usage != nil {
			ir.MergeUsage(&e.usage, *ev.Usage)
		}
		// 本协议把 stop_reason 与 usage 都放在终止帧的 response 对象里，
		// 没有对应的中间帧。
		return nil, nil

	case ir.EvMessageStop:
		return e.finish()

	case ir.EvPing:
		return nil, nil

	case ir.EvError:
		e.errored = true
		return RenderStreamError(ev.Err), nil

	default:
		return nil, nil
	}
}

// openBlock 发条目开启帧。文本块要额外发一层 content_part.added，
// 函数调用与推理条目没有 part 层。
func (e *streamEncoder) openBlock(index int, kind ir.BlockType, block *ir.Block) ([][]byte, error) {
	if _, exists := e.items[index]; exists {
		return nil, nil
	}
	item := &openItem{outputIndex: e.nextOutput, kind: kind}
	e.nextOutput++
	e.items[index] = item
	e.order = append(e.order, index)

	if block != nil && block.ToolUse != nil {
		item.callID = block.ToolUse.ID
		item.name = block.ToolUse.Name
	}

	out, err := e.frame(evOutputItemAdded, wireStreamEvent{
		Type:        evOutputItemAdded,
		OutputIndex: item.outputIndex,
		Item:        item.wire(""),
	})
	if err != nil {
		return nil, err
	}
	if kind != ir.BlockText {
		return out, nil
	}
	part, err := e.frame(evContentPartAdded, wireStreamEvent{
		Type:        evContentPartAdded,
		OutputIndex: item.outputIndex,
		Part:        &wirePart{Type: partOutputText},
	})
	if err != nil {
		return nil, err
	}
	item.partOpen = true
	return append(out, part...), nil
}

// ensureOpen 在 delta 先于块开启帧到达时补开条目。
func (e *streamEncoder) ensureOpen(index int, kind ir.BlockType) ([][]byte, *openItem, error) {
	if item, ok := e.items[index]; ok {
		return nil, item, nil
	}
	out, err := e.openBlock(index, kind, nil)
	if err != nil {
		return nil, nil, err
	}
	return out, e.items[index], nil
}

func (e *streamEncoder) closeBlock(index int) ([][]byte, error) {
	item, ok := e.items[index]
	if !ok || item.closed {
		return nil, nil
	}
	item.closed = true

	var out [][]byte
	if item.partOpen {
		frames, err := e.frame(evContentPartDone, wireStreamEvent{
			Type:        evContentPartDone,
			OutputIndex: item.outputIndex,
			Part:        &wirePart{Type: partOutputText, Text: item.text},
		})
		if err != nil {
			return nil, err
		}
		out = append(out, frames...)
	}
	frames, err := e.frame(evOutputItemDone, wireStreamEvent{
		Type:        evOutputItemDone,
		OutputIndex: item.outputIndex,
		Item:        item.wire("completed"),
	})
	if err != nil {
		return nil, err
	}
	return append(out, frames...), nil
}

// finish 闭合残留条目，再发带完整 response 对象的终止帧。
// 已用 error 帧收尾时不再补：那一帧就是本协议的终止形态。
func (e *streamEncoder) finish() ([][]byte, error) {
	if e.stopped || e.errored {
		return nil, nil
	}
	e.stopped = true

	var out [][]byte
	for _, index := range e.order {
		frames, err := e.closeBlock(index)
		if err != nil {
			return nil, err
		}
		out = append(out, frames...)
	}

	status, incomplete := renderStatus(e.stopReason)
	resp := e.snapshot(status)
	resp.IncompleteDetails = incomplete
	u := renderUsage(e.usage)
	resp.Usage = &u

	kind := evCompleted
	if status == "incomplete" {
		kind = evIncomplete
	}
	frames, err := e.frame(kind, wireStreamEvent{Type: kind, Response: resp})
	if err != nil {
		return nil, err
	}
	return append(out, frames...), nil
}

func (e *streamEncoder) Finish() [][]byte {
	out, err := e.finish()
	if err != nil {
		return nil
	}
	return out
}

// snapshot 组出当前的 response 对象。终止帧必须带它：
// 客户端 SDK 从这里取最终结果，而不是靠自己拼增量。
func (e *streamEncoder) snapshot(status string) *wireResponse {
	out := &wireResponse{
		ID:     e.messageID(),
		Object: "response",
		Model:  e.model,
		Status: status,
	}
	for _, index := range e.order {
		if item := e.items[index]; item != nil {
			out.Output = append(out.Output, *item.wire("completed"))
		}
	}
	return out
}

func (e *streamEncoder) messageID() string {
	if e.id == "" {
		return "resp_unknown"
	}
	return e.id
}

func (e *streamEncoder) frame(kind string, payload wireStreamEvent) ([][]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return [][]byte{codec.EncodeFrame(kind, data)}, nil
}

// wire 把条目状态转成 wire 形态。status 为空表示条目刚开启。
func (i *openItem) wire(status string) *wireItem {
	out := &wireItem{Status: status}
	switch i.kind {
	case ir.BlockToolUse:
		out.Type = itemFunctionCall
		out.CallID = i.callID
		out.Name = i.name
		out.Arguments = i.args
	case ir.BlockThinking:
		out.Type = itemReasoning
		out.EncryptedContent = i.signature
		if i.text != "" {
			out.Summary = []wireSummary{{Type: partSummaryText, Text: i.text}}
		}
	default:
		out.Type = itemMessage
		out.Role = roleAssistant
		parts := []wirePart{}
		if i.text != "" {
			parts = append(parts, wirePart{Type: partOutputText, Text: i.text})
		}
		content, err := json.Marshal(parts)
		if err == nil {
			out.Content = content
		}
	}
	return out
}

// EncodeResponse 编非流式响应体。
func EncodeResponse(resp *ir.Response) ([]byte, error) {
	if resp == nil {
		return nil, nil
	}
	status, incomplete := renderStatus(resp.StopReason)
	u := renderUsage(resp.Usage)
	out := wireResponse{
		ID:                resp.ID,
		Object:            "response",
		Model:             resp.Model,
		Status:            status,
		IncompleteDetails: incomplete,
		Usage:             &u,
	}
	if out.ID == "" {
		out.ID = "resp_unknown"
	}

	var parts []wirePart
	flush := func() error {
		if len(parts) == 0 {
			return nil
		}
		content, err := json.Marshal(parts)
		if err != nil {
			return err
		}
		out.Output = append(out.Output, wireItem{
			Type: itemMessage, Role: roleAssistant, Status: "completed", Content: content,
		})
		parts = nil
		return nil
	}

	for _, b := range resp.Content {
		switch b.Type {
		case ir.BlockText:
			parts = append(parts, wirePart{Type: partOutputText, Text: b.Text})
		case ir.BlockThinking:
			// 推理条目要排在它所解释的输出之前，所以先把攒着的文本条目发出去。
			if err := flush(); err != nil {
				return nil, err
			}
			if b.Thinking == nil {
				continue
			}
			item := wireItem{Type: itemReasoning, Status: "completed"}
			if b.Thinking.Text != "" {
				item.Summary = []wireSummary{{Type: partSummaryText, Text: b.Thinking.Text}}
			}
			if b.Thinking.SignatureFrom == Name {
				item.EncryptedContent = b.Thinking.Signature
			}
			out.Output = append(out.Output, item)
		case ir.BlockToolUse:
			if err := flush(); err != nil {
				return nil, err
			}
			if b.ToolUse == nil {
				continue
			}
			args := b.ToolUse.Input
			if !json.Valid([]byte(args)) {
				args = "{}"
			}
			out.Output = append(out.Output, wireItem{
				Type: itemFunctionCall, Status: "completed",
				CallID: b.ToolUse.ID, Name: b.ToolUse.Name, Arguments: args,
			})
		default:
			return nil, fmt.Errorf("responses: cannot encode block type %q", b.Type)
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

// RenderError 编非流式错误响应。
func RenderError(err *ir.Error) (int, []byte) {
	status, env := errorEnvelope(err)
	body, marshalErr := json.Marshal(env)
	if marshalErr != nil {
		return status, []byte(`{"error":{"type":"server_error","message":"internal error"}}`)
	}
	return status, body
}

// RenderStreamError 编流内错误帧。HTTP 200 已写出，状态码不可再改，
// 错误只能作为流内的 error 事件表达。
func RenderStreamError(err *ir.Error) [][]byte {
	_, env := errorEnvelope(err)
	data, marshalErr := json.Marshal(wireStreamEvent{
		Type:    evError,
		Code:    env.Error.Code,
		Message: env.Error.Message,
		Param:   env.Error.Param,
	})
	if marshalErr != nil {
		return nil
	}
	return [][]byte{codec.EncodeFrame(evError, data)}
}

func errorEnvelope(err *ir.Error) (int, wireErrorEnvelope) {
	if err == nil {
		err = ir.NewError(ir.ErrInternal, 500, "", "unknown error")
	}
	status := err.StatusCode
	if status < 400 {
		status = codec.StatusForKind(err.Kind)
	}
	return status, wireErrorEnvelope{Error: wireError{
		Type:    errorTypeForKind(err.Kind),
		Code:    string(err.Kind),
		Message: err.Message,
	}}
}

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
	default:
		return "server_error"
	}
}
