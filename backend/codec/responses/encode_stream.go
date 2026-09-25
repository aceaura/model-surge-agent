package responses

import (
	"encoding/json"
	"fmt"
	"time"

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
	// created 是写进每个 response 对象的 created_at。构造时取本地钟，
	// 上游在 EvMessageStart 给过真实创建时间就原值覆盖（口径同 ir）。
	created int64

	// items 是已开启的条目，按 IR 块索引定位。
	items map[int]*openItem
	order []int
	// nextOutput 分配 output_index。IR 的块索引不能直接用：
	// 客户端 SDK 要求 output_index 从 0 连续递增。
	nextOutput int

	stopReason ir.StopReason
	usage      ir.Usage
	// serviceTier 是上游回的执行档位，随 snapshot 一并写进 response 对象。
	serviceTier string
	// droppedImages / droppedFiles 被跳过的模型产出附件块数（image /
	// audio+document+file）。本族编码器给助手回合输出的 part 只有
	// output_text / refusal，没有附件形态。不显式拦住会落进 default(text)
	// 分支，凭空多出一个 content 为 [] 的空 message item：客户端读到一条
	// 没有内容的助手消息，它还占掉一个 output_index，把后续真块的序号
	// 一起推后。计数在 Notes() 收尾时报出。
	droppedImages int
	droppedFiles  int
	// droppedServerCalls / droppedServerResults 被跳过的托管工具块数：
	// 本族编码器没有为 Anthropic 的 server_tool_use /
	// web_search_tool_result 输出任何对应 item，同上。两种块型分开计数，
	// 注记只渲染非零的那半。
	droppedServerCalls   int
	droppedServerResults int
	// badToolArgs 是关块时判定畸形的函数调用入参数（增量已发出、改写
	// 不了，只能计数），Notes() 报出。
	badToolArgs int
	// skipIdx 记录被整块跳过的服务端托管工具块索引。跳过发生在开条目
	// 之前，output_index 因此不被烧掉；但该块后续的查询串增量仍会经
	// EvToolInput 到来，不挡住会被 ensureOpen 补开成一个凭空的条目。
	skipIdx map[int]bool
	// notes 是响应侧丢弃说明，累加后由 Notes 去重排序交出。
	notes []string
}

// Notes 实现 codec.StreamNotes。
func (e *streamEncoder) Notes() []string {
	notes := e.notes
	if e.droppedImages > 0 || e.droppedFiles > 0 {
		notes = append(notes, codec.MediaOutputDropNote(e.droppedImages, e.droppedFiles))
	}
	if e.droppedServerCalls > 0 || e.droppedServerResults > 0 {
		notes = append(notes, codec.ServerToolDropNote(e.droppedServerCalls, e.droppedServerResults))
	}
	if e.badToolArgs > 0 {
		notes = append(notes, ir.RawArgsPassNote(e.badToolArgs))
	}
	return codec.DedupeNotes(notes)
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
	return &streamEncoder{items: map[int]*openItem{}, skipIdx: map[int]bool{}, created: time.Now().Unix()}
}

func (e *streamEncoder) Encode(ev ir.Event) ([][]byte, error) {
	if e.errored {
		return nil, nil
	}
	switch ev.Type {
	case ir.EvMessageStart:
		e.id = ev.MessageID
		e.model = ev.Model
		// 档位要在 snapshot 之前收下：created 帧里的 response 对象就该带它。
		if ev.ServiceTier != "" {
			e.serviceTier = ev.ServiceTier
		}
		// 上游给过创建时间就原值回写（覆盖构造时的本地钟）；没给才用本地钟。
		if ev.Created != 0 {
			e.created = ev.Created
		}
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
		switch kind {
		case ir.BlockImage:
			// 模型产出的附件没有本族输出形态：整块跳过但计数，Notes() 报出。
			e.droppedImages++
			return nil, nil
		case ir.BlockAudio, ir.BlockDocument, ir.BlockFile:
			e.droppedFiles++
			return nil, nil
		case ir.BlockServerToolUse:
			// 服务端托管工具块没有本族输出形态（官方虽有 web_search_call
			// item，本服务未实现映射）：整块跳过但计数，Notes() 报出。
			// 且不进 openBlock——那会烧掉一个 output_index 把后续真块序号
			// 推后。记下索引，后续查询串增量一并丢弃，否则会被 ensureOpen
			// 补开成一个凭空的条目。
			e.skipIdx[ev.Index] = true
			e.droppedServerCalls++
			return nil, nil
		case ir.BlockWebSearchToolResult:
			// 搜回来的页面同理：跳过但计数。
			e.skipIdx[ev.Index] = true
			e.droppedServerResults++
			return nil, nil
		}
		return e.openBlock(ev.Index, kind, ev.Block)

	case ir.EvTextDelta:
		if e.skipIdx[ev.Index] {
			return nil, nil
		}
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
		if e.skipIdx[ev.Index] {
			return nil, nil
		}
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
		// 在攒之前判而不是闭合时再判：闭合处已经有一处 encrypted_content
		// 赋值，那里再判一次就有两处判定，一旦漂移会出现「报了丢弃却发了出去」。
		if note, drop := codec.ForeignEventSignature(ev.Text, ev.SignatureFrom, Name); drop {
			e.notes = append(e.notes, note)
			return nil, nil
		}
		// 本协议的 encrypted_content 只能整体给出，没有增量帧，
		// 所以攒进条目，等闭合时随 item 一起发出。
		if item, ok := e.items[ev.Index]; ok {
			item.signature += ev.Text
		}
		return nil, nil

	case ir.EvToolInput:
		if e.skipIdx[ev.Index] {
			// 托管工具块的查询串走同一条增量通道：整块已跳过，
			// 这里必须一起丢，否则 ensureOpen 会把它补开成一个凭空的条目。
			return nil, nil
		}
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
		// 上游可能只在收尾帧给档位。
		if ev.ServiceTier != "" {
			e.serviceTier = ev.ServiceTier
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
		// 先把已开的条目标成 incomplete 闭合，再发 error，最后补 response.failed。
		//
		// 本协议的失败终态是 response.failed：只发 error 帧客户端会一直等一个
		// 终态事件。但绝不发 response.completed——那会把失败说成成功。
		out := e.closeAllAsIncomplete()
		e.errored = true
		out = append(out, RenderStreamError(ev.Err)...)
		return append(out, e.failedFrames(ev.Err)...), nil

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
	return e.closeBlockAs(index, "completed")
}

// closeBlockAs 闭合条目并指定它的终态。
//
// 错误收尾时传 incomplete 而非 completed：那一刻条目里的函数入参可能只有
// 半截 JSON、推理可能缺签名，标成 completed 等于告诉客户端这个条目可以用。
func (e *streamEncoder) closeBlockAs(index int, status string) ([][]byte, error) {
	item, ok := e.items[index]
	if !ok || item.closed {
		return nil, nil
	}
	item.closed = true
	// 关块时校验累积的入参：增量已发出、改写不了，畸形的（多为
	// max_tokens 截断）只能计数报出，让客户端知道这次调用的参数不能
	// 安全执行，而不是看起来以空对象正常完成。
	if item.kind == ir.BlockToolUse {
		if _, valid := ir.NormalizeToolInput([]byte(item.args)); !valid {
			e.badToolArgs++
		}
	}

	var out [][]byte
	// part 级终止帧，官方顺序是 *.done -> content_part.done -> output_item.done。
	// 只发 output_item.done 的话，按 part 事件关块的下游永远等不到块结束
	// （cc-switch 把 output_text.done 直接映射成 content_block_stop）。
	// done 帧必须带完整终态：只读终态不拼增量的下游从这些帧里取内容。
	if item.partOpen {
		done, err := e.frame(evOutputTextDone, wireStreamEvent{
			Type:        evOutputTextDone,
			OutputIndex: item.outputIndex,
			Text:        item.text,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, done...)
		part, err := e.frame(evContentPartDone, wireStreamEvent{
			Type:        evContentPartDone,
			OutputIndex: item.outputIndex,
			Part:        &wirePart{Type: partOutputText, Text: item.text},
		})
		if err != nil {
			return nil, err
		}
		out = append(out, part...)
	}
	if item.kind == ir.BlockThinking {
		sumDone, err := e.frame(evReasoningSummaryTextDone, wireStreamEvent{
			Type:        evReasoningSummaryTextDone,
			OutputIndex: item.outputIndex,
			Text:        item.text,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, sumDone...)
		partDone, err := e.frame(evReasoningSummaryPartDone, wireStreamEvent{
			Type:        evReasoningSummaryPartDone,
			OutputIndex: item.outputIndex,
			Part:        &wirePart{Type: partSummaryText, Text: item.text},
		})
		if err != nil {
			return nil, err
		}
		out = append(out, partDone...)
	}
	frames, err := e.frame(evOutputItemDone, wireStreamEvent{
		Type:        evOutputItemDone,
		OutputIndex: item.outputIndex,
		Item:        item.wire(status),
	})
	if err != nil {
		return nil, err
	}
	return append(out, frames...), nil
}

// closeAllAsIncomplete 闭合所有残留条目，供错误收尾使用。
func (e *streamEncoder) closeAllAsIncomplete() [][]byte {
	var out [][]byte
	for _, index := range e.order {
		frames, err := e.closeBlockAs(index, "incomplete")
		if err != nil {
			continue
		}
		out = append(out, frames...)
	}
	return out
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

// failedFrames 发 response.failed，本协议的失败终态。
//
// response 对象里不重复带已发出的 output items：客户端已经逐帧收到过它们，
// 再来一份不增加信息，而为此在编码器里维护一份副本等于把聚合职责搬进来。
func (e *streamEncoder) failedFrames(err *ir.Error) [][]byte {
	resp := &wireResponse{
		ID:     e.messageID(),
		Object: "response",
		Model:  e.model,
		Status: "failed",
	}
	_, env := errorEnvelope(err)
	resp.Error = &env.Error
	u := renderUsage(e.usage)
	resp.Usage = &u
	frames, frameErr := e.frame(evFailed, wireStreamEvent{Type: evFailed, Response: resp})
	if frameErr != nil {
		return nil
	}
	return frames
}

// snapshot 组出当前的 response 对象。终止帧必须带它：
// 客户端 SDK 从这里取最终结果，而不是靠自己拼增量。
func (e *streamEncoder) snapshot(status string) *wireResponse {
	out := &wireResponse{
		ID:          e.messageID(),
		Object:      "response",
		Model:       e.model,
		Status:      status,
		CreatedAt:   e.created,
		ServiceTier: e.serviceTier,
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
	// 补齐放在这个单一漏斗处：所有流帧都经过 frame，补一次即全覆盖。
	// 不去掉 wireStreamEvent 的 omitempty：同一结构体也用于入站解码，
	// 且序号字段并非每个事件类型都有，无条件写出会造出本协议里不存在的形状。
	data, err = backfillIndexFields(kind, data)
	if err != nil {
		return nil, err
	}
	return [][]byte{codec.EncodeFrame(kind, data)}, nil
}

// backfillIndexFields 给该事件类型声明的序号字段补上缺失的零值。
func backfillIndexFields(kind string, data []byte) ([]byte, error) {
	required := requiredIndexFields[kind]
	if len(required) == 0 {
		return data, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, err
	}
	missing := false
	for _, f := range required {
		if _, ok := obj[f]; !ok {
			obj[f] = json.RawMessage("0")
			missing = true
		}
	}
	if !missing {
		return data, nil
	}
	return json.Marshal(obj)
}

// wire 把条目状态转成 wire 形态。status 为空表示条目刚开启。
func (i *openItem) wire(status string) *wireRespItem {
	out := &wireRespItem{Status: status}
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
	// 上游给过创建时间就原值回写；没给才回退本地钟（同 chat 侧口径）。
	created := resp.Created
	if created == 0 {
		created = time.Now().Unix()
	}
	out := wireResponse{
		ID:                resp.ID,
		Object:            "response",
		Model:             resp.Model,
		Status:            status,
		CreatedAt:         created,
		IncompleteDetails: incomplete,
		Usage:             &u,
		ServiceTier:       resp.ServiceTier,
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
		out.Output = append(out.Output, wireRespItem{
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
			item := wireRespItem{Type: itemReasoning, Status: "completed"}
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
			// arguments 是字符串槽位：畸形原文照转义嵌入，响应体不会因此
			// 非法。不清空成 {}——那会让客户端把参数损坏的调用当无参调用
			// 存进历史，损耗由 EncodeResponseLossy 报出。
			args := b.ToolUse.Input
			out.Output = append(out.Output, wireRespItem{
				Type: itemFunctionCall, Status: "completed",
				CallID: b.ToolUse.ID, Name: b.ToolUse.Name, Arguments: args,
			})
		case ir.BlockImage, ir.BlockAudio, ir.BlockDocument, ir.BlockFile:
			// 助手回合的 output 条目没有附件形态：整块跳过，损耗由
			// EncodeResponseLossy 经 CountResponseMedia 报出。编不出去还硬编
			// 就是 default 分支那条路——凭空造一个空 message 条目。
			continue
		case ir.BlockServerToolUse, ir.BlockWebSearchToolResult:
			// 服务端托管工具块没有本族输出条目形态：整块跳过。落进 default
			// 会硬报错，编成 message 条目则凭空多出一个空助手消息。
			// 跳过，损耗由 EncodeResponseLossy 经 CountResponseServerTools 报出。
			continue
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
		Param:   err.Param,
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
