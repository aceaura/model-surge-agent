package chatcompletions

import (
	"encoding/json"
	"time"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// streamEncoder 把 IR 事件编成 chat.completion.chunk 序列。
//
// IR 的块模型要压回本协议的隐式续写形态：块索引在这里被丢弃，
// 只保留「这个 delta 落在正文、推理还是某个工具调用上」。工具调用是唯一
// 需要保留序号的：客户端靠 tool_calls[].index 拼回分片，缺了会中断回合。
type streamEncoder struct {
	id      string
	model   string
	created int64
	stopped bool
	// errored 表示流已用错误帧收尾。此后不再发 finish_reason/usage/[DONE]，
	// 也丢弃迟到的增量：那些帧会让客户端把残缺回答当成正常结束存下来。
	errored bool
	// blockKind 记录每个块索引的类型，delta 到来时据此决定落点。
	blockKind map[int]ir.BlockType
	// toolIndex 把块索引映射成连续的 tool_calls 序号。
	// IR 的块索引里混着文本块与推理块，不能直接当工具序号用。
	toolIndex map[int]int
	nextTool  int
	// sentToolHeader 记录某个工具调用的 id/name 是否已发过：
	// 本协议只在首片带这两个字段，重复发送会让部分客户端建出两个调用。
	sentToolHeader map[int]bool
	stopReason     ir.StopReason
	// usage 跨帧累积：input 与 output 可能来自不同的 IR 事件。
	usage ir.Usage
	// notes 是响应侧丢弃说明，累加后由 Notes 去重排序交出。
	notes []string
}

// Notes 实现 codec.StreamNotes。
func (e *streamEncoder) Notes() []string { return codec.DedupeNotes(e.notes) }

func newStreamEncoder() *streamEncoder {
	return &streamEncoder{
		blockKind:      map[int]ir.BlockType{},
		toolIndex:      map[int]int{},
		sentToolHeader: map[int]bool{},
		created:        time.Now().Unix(),
	}
}

func (e *streamEncoder) Encode(ev ir.Event) ([][]byte, error) {
	if e.errored {
		return nil, nil
	}
	switch ev.Type {
	case ir.EvMessageStart:
		e.id = ev.MessageID
		e.model = ev.Model
		// Anthropic 上游在这一帧给 input_tokens，而本协议只有末尾一帧 usage，
		// 不在这里收下就永远丢了。
		if ev.Usage != nil {
			ir.MergeUsage(&e.usage, *ev.Usage)
		}
		// 本协议的首帧就是一个带 role 的 delta，没有独立的消息头帧。
		return e.chunk(wireMessage{Role: roleAssistant}, "")

	case ir.EvBlockStart:
		kind := ir.BlockText
		if ev.Block != nil {
			kind = ev.Block.Type
		}
		e.blockKind[ev.Index] = kind
		if kind != ir.BlockToolUse {
			return nil, nil
		}
		// 工具调用的块开启帧带 id 与 name，必须立刻发出：
		// 后续只有 arguments 分片，此时不发就永远发不出去了。
		n := e.toolSlot(ev.Index)
		call := wireToolCall{Index: &n, Type: "function"}
		if ev.Block != nil && ev.Block.ToolUse != nil {
			call.ID = ev.Block.ToolUse.ID
			call.Function.Name = ev.Block.ToolUse.Name
		}
		e.sentToolHeader[ev.Index] = true
		return e.chunk(wireMessage{ToolCalls: []wireToolCall{call}}, "")

	case ir.EvTextDelta:
		content, err := json.Marshal(ev.Text)
		if err != nil {
			return nil, err
		}
		return e.chunk(wireMessage{Content: content}, "")

	case ir.EvThinkingDelta:
		return e.chunk(wireMessage{ReasoningContent: ev.Text}, "")

	case ir.EvSigDelta:
		// 本协议没有承载推理签名的字段，一律丢弃，因此天然同族安全——
		// 不做同族判定不是遗漏：判出同族也无处可放，异族与同族的处置相同。
		// 但丢弃要上报，否则客户端看不到签名时无从知道是协议限制还是上游没给。
		if ev.Text != "" {
			e.notes = append(e.notes, codec.ResponseSignatureUnsupported(Name))
		}
		return nil, nil

	case ir.EvToolInput:
		n := e.toolSlot(ev.Index)
		call := wireToolCall{Index: &n, Function: wireFunctionCall{Arguments: ev.Text}}
		if !e.sentToolHeader[ev.Index] {
			// 上游漏发块开启帧时补上 type，否则客户端不知道这是函数调用。
			call.Type = "function"
			e.sentToolHeader[ev.Index] = true
		}
		return e.chunk(wireMessage{ToolCalls: []wireToolCall{call}}, "")

	case ir.EvBlockStop:
		// 本协议无块边界概念，块闭合无需表达。
		return nil, nil

	case ir.EvMessageDelta:
		if ev.StopReason != "" {
			e.stopReason = ev.StopReason
		}
		if ev.Usage != nil {
			ir.MergeUsage(&e.usage, *ev.Usage)
		}
		return nil, nil

	case ir.EvMessageStop:
		return e.finish()

	case ir.EvPing:
		// 保活在本协议里靠 SSE 注释行，无对应的 chunk，丢弃。
		return nil, nil

	case ir.EvError:
		e.errored = true
		return RenderStreamError(ev.Err), nil

	default:
		return nil, nil
	}
}

// finish 发终止三件套：带 finish_reason 的空 delta、单独的 usage 帧、[DONE]。
// usage 单独成帧是本协议 stream_options.include_usage 的约定形态：
// 那一帧的 choices 为空数组。
func (e *streamEncoder) finish() ([][]byte, error) {
	// 错误帧已自带 [DONE]，这里再发一套会让客户端读到两个终止。
	if e.stopped || e.errored {
		return nil, nil
	}
	e.stopped = true

	out, err := e.chunk(wireMessage{}, renderFinishReason(e.stopReason))
	if err != nil {
		return nil, err
	}
	u := renderUsage(e.usage)
	frame, err := e.marshal(wireResponse{
		ID: e.messageID(), Object: chunkObject, Created: e.created,
		Model: e.model, Choices: []wireChoice{}, Usage: &u,
	})
	if err != nil {
		return nil, err
	}
	out = append(out, frame)
	return append(out, codec.EncodeFrame("", []byte(doneSentinel))), nil
}

func (e *streamEncoder) Finish() [][]byte {
	out, err := e.finish()
	if err != nil {
		return nil
	}
	return out
}

// toolSlot 把块索引映射成连续的工具调用序号。
func (e *streamEncoder) toolSlot(blockIndex int) int {
	if n, ok := e.toolIndex[blockIndex]; ok {
		return n
	}
	n := e.nextTool
	e.nextTool++
	e.toolIndex[blockIndex] = n
	return n
}

func (e *streamEncoder) chunk(delta wireMessage, finish string) ([][]byte, error) {
	frame, err := e.marshal(wireResponse{
		ID: e.messageID(), Object: chunkObject, Created: e.created, Model: e.model,
		Choices: []wireChoice{{Index: 0, Delta: &delta, FinishReason: finish}},
	})
	if err != nil {
		return nil, err
	}
	return [][]byte{frame}, nil
}

func (e *streamEncoder) marshal(resp wireResponse) ([]byte, error) {
	data, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return codec.EncodeFrame("", data), nil
}

func (e *streamEncoder) messageID() string {
	if e.id == "" {
		return "chatcmpl-unknown"
	}
	return e.id
}

// EncodeResponse 编非流式响应体。
func EncodeResponse(resp *ir.Response) ([]byte, error) {
	if resp == nil {
		return nil, nil
	}
	msg := wireMessage{Role: roleAssistant}
	var (
		text  []ir.Block
		calls []wireToolCall
	)
	for _, b := range resp.Content {
		switch b.Type {
		case ir.BlockThinking:
			if b.Thinking != nil {
				msg.ReasoningContent += b.Thinking.Text
			}
		case ir.BlockToolUse:
			if b.ToolUse == nil {
				continue
			}
			args := b.ToolUse.Input
			if !json.Valid([]byte(args)) {
				args = "{}"
			}
			n := len(calls)
			calls = append(calls, wireToolCall{
				Index: &n, ID: b.ToolUse.ID, Type: "function",
				Function: wireFunctionCall{Name: b.ToolUse.Name, Arguments: args},
			})
		default:
			text = append(text, b)
		}
	}
	content, err := encodeContent(text)
	if err != nil {
		return nil, err
	}
	msg.Content = content
	msg.ToolCalls = calls

	u := renderUsage(resp.Usage)
	out := wireResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   resp.Model,
		Choices: []wireChoice{{
			Index: 0, Message: &msg, FinishReason: renderFinishReason(resp.StopReason),
		}},
		Usage: &u,
	}
	if out.ID == "" {
		out.ID = "chatcmpl-unknown"
	}
	return json.Marshal(out)
}

// RenderError 编非流式错误响应。
func RenderError(err *ir.Error) (int, []byte) {
	status, env := errorEnvelope(err)
	body, marshalErr := json.Marshal(env)
	if marshalErr != nil {
		return status, []byte(`{"error":{"message":"internal error","type":"server_error"}}`)
	}
	return status, body
}

// RenderStreamError 编流内错误。HTTP 200 已写出，状态码不可再改，
// 错误只能作为一帧内含 error 的 data 送出，随后补 [DONE] 让客户端收束。
func RenderStreamError(err *ir.Error) [][]byte {
	_, env := errorEnvelope(err)
	body, marshalErr := json.Marshal(env)
	if marshalErr != nil {
		return nil
	}
	return [][]byte{
		codec.EncodeFrame("", body),
		codec.EncodeFrame("", []byte(doneSentinel)),
	}
}

func errorEnvelope(err *ir.Error) (int, wireErrorEnvelope) {
	if err == nil {
		err = ir.NewError(ir.ErrInternal, 500, "", "unknown error")
	}
	status := err.StatusCode
	if status < 400 {
		status = codec.StatusForKind(err.Kind)
	}
	code, _ := json.Marshal(string(err.Kind))
	return status, wireErrorEnvelope{Error: wireError{
		Message: err.Message,
		Type:    errorTypeForKind(err.Kind),
		Code:    code,
	}}
}

// errorTypeForKind 用 OpenAI 的错误类型名，让客户端 SDK 能按自己的分类处理。
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
