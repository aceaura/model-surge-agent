package chatcompletions

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
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
	// slots 把语义槽位映射到块索引。键是 "text" / "reasoning"。
	// 工具调用不走这里：它要缓冲入参并延迟宣告，状态见 toolSlots。
	slots map[string]int
	// order 记录槽位分配顺序，用于结束时按开启顺序闭合。
	order []int
	next  int

	// toolSlots 按上游给的 index 索引工具调用槽位。
	toolSlots map[int]*toolSlot
	// byID 按调用 id 索引槽位。有些兼容实现同一次调用的分片带着不同的
	// index（每帧重新从 0 编号，或按 choice 内序号而非调用序号给），
	// 只看 index 会把一次调用拆成两个块，客户端就会重复执行同一个工具。
	// 空 id 不入索引：多个空 id 会互相误合并，把不同调用的入参串成一份非法 JSON。
	byID map[string]*toolSlot
	// callCounter 用于给没带 id 的调用合成一个。
	callCounter int
	// messageID 是上游这次响应的 id，作为合成 id 的 scope。
	// 缺它则两轮的同名调用会拿到同一个合成 id，见 codec.SynthToolID。
	messageID string
	// notes 记录改写说明，走响应侧诊断通道。
	notes []string
	// maxCandidate 是见过的最大候选索引。只记最大值而不逐帧记说明：
	// 说明按字符串去重，逐帧生成会让同一个流报出好几条不同数字的说明。
	maxCandidate int
	// serviceTier 是上游回的执行档位，随每个 chunk 顶层给出。
	serviceTier string

	// stopReason 与 usage 先攒着，到 [DONE] 才发一帧 message_delta。
	// 本协议把它们分散在不同 chunk（finish_reason 一帧、usage 另一帧），
	// 各自发一帧会让下游收到两个 message_delta，Anthropic 客户端不接受。
	stopReason ir.StopReason
	usage      *ir.Usage
	done       bool
}

// toolSlot 是一个正在累积的工具调用。
//
// 必须缓冲而不能边收边发：block_start 只有一次机会带上 id 与 name，
// 而各家实现给这两个字段的时机不同——有的首片就全给，有的先发几片
// arguments 才补上 name。宣告前把 arguments 攒在 pending 里，
// 等 id 与 name 都到齐再一次性放出。
type toolSlot struct {
	index     int
	id        string
	name      string
	pending   string
	announced bool
}

func newStreamDecoder() *streamDecoder {
	return &streamDecoder{
		slots:     map[string]int{},
		toolSlots: map[int]*toolSlot{},
		byID:      map[string]*toolSlot{},
	}
}

func (d *streamDecoder) Feed(event, data string) ([]ir.Event, error) {
	out, split, err := codec.FeedWithSplit(event, data, d.feedOne)
	if split {
		d.notes = append(d.notes, codec.MultipleJSONDocsNote)
	}
	return out, err
}

func (d *streamDecoder) feedOne(_, data string) ([]ir.Event, error) {
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

	if chunk.ServiceTier != "" {
		d.serviceTier = chunk.ServiceTier
	}

	var out []ir.Event
	out = append(out, d.ensureStarted(chunk)...)

	if chunk.Usage != nil {
		u := convertUsage(*chunk.Usage)
		d.usage = &u
	}

	for _, choice := range chunk.Choices {
		// 只处理第一路：IR 是单条响应，n>1 的其余路无处安放。
		// 丢是结构决定的，但必须说出来——客户端为全部候选付了 token。
		if choice.Index != 0 {
			if choice.Index > d.maxCandidate {
				d.maxCandidate = choice.Index
			}
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
	d.messageID = chunk.ID
	return []ir.Event{{Type: ir.EvMessageStart, MessageID: chunk.ID, Model: chunk.Model,
		ServiceTier: chunk.ServiceTier}}
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

	out = append(out, d.decodeToolCalls(delta.ToolCalls)...)
	return out, nil
}

// decodeToolCalls 累积工具调用分片。
func (d *streamDecoder) decodeToolCalls(calls []wireToolCall) []ir.Event {
	var out []ir.Event
	for i, tc := range calls {
		// index 是本协议拼回分片的唯一依据；缺失时退回数组下标。
		n := i
		if tc.Index != nil {
			n = *tc.Index
		}

		slot := d.toolSlots[n]
		// id 优先于 index：同一 id 跨 index 到达时并回原槽位，
		// 否则一次调用会被拆成两个块，客户端重复执行同一个工具。
		if tc.ID != "" {
			if byID := d.byID[tc.ID]; byID != nil {
				if slot != byID {
					// 本分片带的 index 与该 id 首次出现时的不同：
					// 归到 id 对应的槽位，并把说明报给客户端——
					// 合并是我们的判断，客户端有权知道上游原样并非如此。
					d.notes = append(d.notes, mergedToolNote)
					if slot == nil {
						// 该 index 还空着：指向同一槽位，后续不带 id 的
						// 分片沿着 index 也能找回来。已被别的调用占着则不动。
						d.toolSlots[n] = byID
					}
					slot = byID
				}
			} else if slot != nil && slot.announced && tc.ID != slot.id {
				// 已宣告的槽位收到另一个非空 id：上游复用了 index 表示新调用，
				// 并进原槽位会把两次调用的入参串成一份非法 JSON。
				slot = nil
			}
		}
		if slot == nil {
			slot = &toolSlot{index: d.allocIndex()}
			d.toolSlots[n] = slot
		}
		// 只有非空值才回填：后续分片的这两个字段通常是空串，覆盖会抹掉首片给的值。
		if tc.ID != "" {
			slot.id = tc.ID
			d.byID[tc.ID] = slot
		}
		if tc.Function.Name != "" {
			slot.name = tc.Function.Name
		}

		if slot.announced {
			if tc.Function.Arguments != "" {
				out = append(out, ir.Event{
					Type: ir.EvToolInput, Index: slot.index, Text: tc.Function.Arguments})
			}
			continue
		}
		slot.pending += tc.Function.Arguments
		if slot.name != "" {
			out = append(out, d.announce(slot)...)
		}
	}
	return out
}

// announce 发出块开启帧并把缓冲的入参一次性放出。
// id 到这一刻仍缺失就合成一个：另外三个协议都要靠它把结果回指到调用。
func (d *streamDecoder) announce(slot *toolSlot) []ir.Event {
	if slot.id == "" {
		slot.id = d.synthCallID(slot.name)
	}
	slot.announced = true
	out := []ir.Event{{
		Type:  ir.EvBlockStart,
		Index: slot.index,
		Block: &ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
			ID:   slot.id,
			Name: slot.name,
		}},
	}}
	if slot.pending != "" {
		out = append(out, ir.Event{Type: ir.EvToolInput, Index: slot.index, Text: slot.pending})
		slot.pending = ""
	}
	return out
}

func (d *streamDecoder) synthCallID(name string) string {
	d.callCounter++
	return codec.SynthToolID(d.messageID, name, d.callCounter)
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
	idx := d.allocIndex()
	d.slots[key] = idx
	return idx
}

func (d *streamDecoder) allocIndex() int {
	idx := d.next
	d.next++
	d.order = append(d.order, idx)
	return idx
}

func (d *streamDecoder) closeAll() []ir.Event {
	out := d.announcePending()
	for _, idx := range d.order {
		out = append(out, ir.Event{Type: ir.EvBlockStop, Index: idx})
	}
	d.order = nil
	return out
}

// announcePending 给流结束时 name 仍未到达的工具槽位补宣告。
// 静默丢掉整个调用更糟：客户端会看到一次没有工具调用的回答，
// 而上游其实已经决定调用工具了。
func (d *streamDecoder) announcePending() []ir.Event {
	pending := make([]*toolSlot, 0, len(d.toolSlots))
	for _, slot := range d.toolSlots {
		if !slot.announced {
			pending = append(pending, slot)
		}
	}
	// map 遍历无序，按块索引排出确定顺序。
	sort.Slice(pending, func(i, j int) bool { return pending[i].index < pending[j].index })

	var out []ir.Event
	for _, slot := range pending {
		if slot.name == "" {
			slot.name = unknownToolName
		}
		// 残缺入参发出去会让整条历史带上语法错误的 JSON。
		if !json.Valid([]byte(slot.pending)) {
			slot.pending = "{}"
		}
		out = append(out, d.announce(slot)...)
	}
	return out
}

// unknownToolName 是 name 始终未到达时的占位名。
const unknownToolName = "unknown_tool"

// mergedToolNote 是同 id 跨 index 合并的说明。
const mergedToolNote = "merged tool call fragments that arrived under different indexes " +
	"(matched by call id)"

// Notes 实现 codec.StreamNotes。
//
// 多候选说明在这里生成而不在 Feed 里 append：数字要的是整流的结论，
// 逐帧 append 会让每帧一条、去重挡不住。
func (d *streamDecoder) Notes() []string {
	notes := d.notes
	if d.maxCandidate > 0 {
		notes = append(notes, codec.DroppedCandidatesNote(d.maxCandidate))
	}
	return codec.DedupeNotes(notes)
}

// finish 收尾：闭合残留块，补一帧带 stop_reason 与 usage 的 message_delta，
// 再发 message_stop。
func (d *streamDecoder) finish() []ir.Event {
	if d.done {
		return nil
	}
	d.done = true
	out := d.closeAll()
	delta := ir.Event{Type: ir.EvMessageDelta, StopReason: d.stopReason, Usage: d.usage,
		ServiceTier: d.serviceTier}
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
	resp, _, err := DecodeResponseLossy(body)
	return resp, err
}

// DecodeResponseLossy 实现 codec.LossyResponseDecoder。
func DecodeResponseLossy(body []byte) (*ir.Response, []string, error) {
	var w wireResponse
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, nil, ir.NewError(ir.ErrUpstream, 0, "",
			fmt.Sprintf("undecodable response: %v", err))
	}
	out := &ir.Response{ID: w.ID, Model: w.Model, Content: []ir.Block{},
		ServiceTier: w.ServiceTier}
	if w.Usage != nil {
		out.Usage = convertUsage(*w.Usage)
	}
	maxCandidate := 0
	for _, choice := range w.Choices {
		if choice.Index != 0 || choice.Message == nil {
			if choice.Index > maxCandidate {
				maxCandidate = choice.Index
			}
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
			return nil, nil, ir.NewError(ir.ErrUpstream, 0, "",
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
	var notes []string
	if maxCandidate > 0 {
		notes = append(notes, codec.DroppedCandidatesNote(maxCandidate))
	}
	return out, notes, nil
}

func DecodeError(status int, header http.Header, body []byte) *ir.Error {
	var env wireErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil || env.Error.Message == "" {
		// 本协议的兼容实现最多，错误体形状五花八门：先尽力挖消息，
		// 挖不到才回落状态码描述。
		return codec.WithRetryAfter(codec.WithParam(codec.FallbackError(status, body), body), header)
	}
	return codec.WithRetryAfter(codec.WithParam(convertError(status, &env.Error), body), header)
}

func convertError(status int, e *wireError) *ir.Error {
	if e == nil {
		return ir.NewError(codec.KindForStatus(status, ""), status, "", "upstream error without detail")
	}
	code := e.Type
	if code == "" {
		code = errorCode(e.Code)
	}
	// 消息位上可能是被字符串化的下游错误体，取出里面的真消息再归类：
	// 上下文超限的判定要看消息文本，读到一串转义引号就判不出来了。
	msg := codec.RefineMessage(e.Message)
	out := ir.NewError(codec.KindFor(status, code, msg), status, code, msg)
	out.Param = e.Param
	return out
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
	// 几种缓存字段名同义，按优先级取第一个非零的。
	if u.PromptTokensDetails != nil {
		out.CacheReadTokens = u.PromptTokensDetails.CachedTokens
	}
	if out.CacheReadTokens == 0 {
		out.CacheReadTokens = u.PromptCacheHitTokens
	}
	if out.CacheReadTokens == 0 {
		out.CacheReadTokens = u.CacheReadInputTokens
	}
	out.CacheWriteTokens = u.CacheWriteTokens
	if out.CacheWriteTokens == 0 {
		out.CacheWriteTokens = u.CacheCreationTokens
	}
	// 本协议的 completion_tokens 已含推理，IR 同口径，故只记维度不做扣减。
	if u.CompletionTokensDetails != nil {
		out.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
	}
	// 本协议的 prompt_tokens 含缓存命中，而 IR 的 InputTokens 定义为
	// 不含缓存的新鲜输入，故减去。上游数字不自洽时钳到 0，不出负数。
	out.InputTokens -= out.CacheReadTokens
	if out.InputTokens < 0 {
		out.InputTokens = 0
	}
	return out
}

func renderUsage(u ir.Usage) wireUsage {
	// 加回缓存命中：本协议的客户端期望 prompt_tokens 是输入总量。
	prompt := u.InputTokens + u.CacheReadTokens
	out := wireUsage{
		PromptTokens:     prompt,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      prompt + u.OutputTokens,
	}
	if u.CacheReadTokens > 0 {
		out.PromptTokensDetails = &wirePromptDetails{CachedTokens: u.CacheReadTokens}
	}
	// 本协议没有官方的缓存写入字段，用兼容层通行的别名给出。
	if u.CacheWriteTokens > 0 {
		out.CacheWriteTokens = u.CacheWriteTokens
	}
	// 本协议表达得了推理维度，如实写出，让客户端看得见推理占比。
	if u.ReasoningTokens > 0 {
		out.CompletionTokensDetails = &wireCompletionDetails{ReasoningTokens: u.ReasoningTokens}
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
	case "":
		// 上游没给：留空由聚合层兜底，不能当成被拦截。
		return ""
	default:
		// 未识别的取值按安全侧兜底：把被拦截的回答当正常结束，
		// 客户端会照着不完整的内容继续往下走。
		return ir.StopContentFilter
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
	case ir.StopEndTurn, ir.StopStopSequence, "":
		return "stop"
	default:
		// 认不出的 IR 取值不能一律说成正常结束：宁可让客户端知道
		// 这次终止有异常，也不要它照着可能残缺的内容继续。
		return "content_filter"
	}
}
