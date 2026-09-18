package gemini

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/codec/ratelimit"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// streamDecoder 把 alt=sse 的帧序列解成 IR 事件。
//
// 本协议的流式模型是「每帧一个完整响应对象」，帧内的 parts 是隐式续写，
// 既没有块生命周期也没有终止标记。所以这里要做三件事：
// 按 part 种类变化切出块边界、为函数调用合成调用 id、在流结束时补终止事件。
type streamDecoder struct {
	started bool
	done    bool

	// current 是当前开着的块。本协议的 parts 没有索引，
	// 只能靠「种类是否变化」判断该续写还是该另起一块。
	current     *openBlock
	nextIndex   int
	messageID   string
	callCounter int
	// sawCall 记录本次响应有没有函数调用。本协议即使以工具调用收尾
	// 也报 STOP，不单独记就会把 tool_use 降级成 end_turn，
	// 客户端会以为回合结束而不去执行工具。
	sawCall bool

	stopReason ir.StopReason
	usage      *ir.Usage
	// notes 记录改写说明，走响应侧诊断通道。
	notes []string
	// maxCandidate 是见过的最大候选索引。只记最大值不逐帧记说明：
	// 说明按字符串去重，逐帧生成会让一个流报出好几条不同数字的说明。
	maxCandidate int
}

type openBlock struct {
	index int
	kind  ir.BlockType
}

func newStreamDecoder() *streamDecoder { return &streamDecoder{} }

// Notes 实现 codec.StreamNotes。
//
// 多候选说明在这里生成而不在 Feed 里 append：数字要的是整流的结论。
func (d *streamDecoder) Notes() []string {
	notes := d.notes
	if d.maxCandidate > 0 {
		notes = append(notes, codec.DroppedCandidatesNote(d.maxCandidate))
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

func (d *streamDecoder) feedOne(_, data string) ([]ir.Event, error) {
	data = strings.TrimSpace(data)
	if data == "" {
		return nil, nil
	}
	var frame wireResponse
	if err := json.Unmarshal([]byte(data), &frame); err != nil {
		return nil, ir.NewError(ir.ErrUpstream, 0, "",
			fmt.Sprintf("undecodable stream frame: %v", err))
	}
	// 错误可能以流内帧的形式出现，而非 HTTP 状态码。
	if frame.Error != nil {
		return []ir.Event{{Type: ir.EvError, Err: convertError(frame.Error.Code, frame.Error)}}, nil
	}

	var out []ir.Event
	if frame.ResponseID != "" {
		d.messageID = frame.ResponseID
	}
	out = append(out, d.start(frame)...)

	if frame.UsageMetadata != nil {
		u := convertUsage(*frame.UsageMetadata)
		if d.usage == nil {
			d.usage = &u
		} else {
			ir.MergeUsage(d.usage, u)
		}
	}
	// 整个请求被安全策略拒了：candidates 为空，只能从 promptFeedback 读出原因。
	if frame.PromptFeedback != nil && frame.PromptFeedback.BlockReason != "" {
		d.stopReason = ir.StopContentFilter
	}

	for _, cand := range frame.Candidates {
		// 只处理第一路：IR 是单条响应，其余候选无处安放。
		// 丢是结构决定的，但必须说出来——客户端为全部候选付了 token。
		if cand.Index != 0 {
			if cand.Index > d.maxCandidate {
				d.maxCandidate = cand.Index
			}
			continue
		}
		if cand.FinishMessage != "" {
			d.notes = append(d.notes, codec.FinishDetailNote(cand.FinishMessage))
		}
		if cand.Content != nil {
			events, err := d.decodeParts(cand.Content.Parts)
			if err != nil {
				return nil, err
			}
			out = append(out, events...)
		}
		if cand.FinishReason != "" {
			d.stopReason = convertFinishReason(cand.FinishReason)
		}
	}
	return out, nil
}

func (d *streamDecoder) start(frame wireResponse) []ir.Event {
	if d.started {
		return nil
	}
	d.started = true
	return []ir.Event{{
		Type:      ir.EvMessageStart,
		MessageID: d.messageID,
		Model:     frame.ModelVersion,
	}}
}

func (d *streamDecoder) decodeParts(parts []wirePart) ([]ir.Event, error) {
	var out []ir.Event
	for _, p := range parts {
		switch {
		case p.FunctionCall != nil:
			// 函数调用不分片：整个 args 在一个 part 里到齐，
			// 所以开块、发入参、闭块可以一次做完。
			events, err := d.emitCall(p.FunctionCall)
			if err != nil {
				return nil, err
			}
			out = append(out, events...)

		case p.Thought:
			out = append(out, d.switchTo(ir.BlockThinking)...)
			if p.Text != "" {
				out = append(out, ir.Event{Type: ir.EvThinkingDelta, Index: d.current.index, Text: p.Text})
			}
			if p.ThoughtSignature != "" {
				out = append(out, ir.Event{
					Type:          ir.EvSigDelta,
					Index:         d.current.index,
					Text:          p.ThoughtSignature,
					SignatureFrom: Name,
				})
			}

		case p.Text != "":
			out = append(out, d.switchTo(ir.BlockText)...)
			out = append(out, ir.Event{Type: ir.EvTextDelta, Index: d.current.index, Text: p.Text})

		case p.InlineData != nil || p.FileData != nil:
			// 模型返回的图片本服务不往下游转：IR 的图片块只用于请求方向，
			// 三个下游协议对响应内图片的表达各不相同且都不通用。
			continue
		}
	}
	return out, nil
}

// switchTo 保证当前开着的块是指定种类：种类变了就先闭合旧块再开新块。
// 本协议的 parts 没有索引，块边界只能这样推出来。
func (d *streamDecoder) switchTo(kind ir.BlockType) []ir.Event {
	if d.current != nil && d.current.kind == kind {
		return nil
	}
	var out []ir.Event
	out = append(out, d.closeCurrent()...)

	index := d.nextIndex
	d.nextIndex++
	d.current = &openBlock{index: index, kind: kind}

	block := ir.Block{Type: kind}
	if kind == ir.BlockThinking {
		block.Thinking = &ir.Thinking{SignatureFrom: Name}
	}
	return append(out, ir.Event{Type: ir.EvBlockStart, Index: index, Block: &block})
}

func (d *streamDecoder) closeCurrent() []ir.Event {
	if d.current == nil {
		return nil
	}
	index := d.current.index
	d.current = nil
	return []ir.Event{{Type: ir.EvBlockStop, Index: index}}
}

// emitCall 把一个完整的函数调用发成开块、入参、闭块三个事件。
func (d *streamDecoder) emitCall(call *wireFunctionCall) ([]ir.Event, error) {
	out := d.closeCurrent()
	d.sawCall = true

	index := d.nextIndex
	d.nextIndex++
	args := string(call.Args)
	if !json.Valid([]byte(args)) {
		args = "{}"
	}
	out = append(out,
		ir.Event{Type: ir.EvBlockStart, Index: index, Block: &ir.Block{
			Type:    ir.BlockToolUse,
			ToolUse: &ir.ToolUse{ID: d.callID(call), Name: call.Name},
		}},
		ir.Event{Type: ir.EvToolInput, Index: index, Text: args},
		ir.Event{Type: ir.EvBlockStop, Index: index},
	)
	return out, nil
}

// callID 取上游给的 id，没有就合成一个。
//
// 本协议的 functionCall 通常只有 name，而另外三个协议都要求调用 id
// 才能把结果回指到调用。合成的 id 会随响应发给客户端，客户端下一轮带回来，
// 编码请求时再由 id→name 表翻回名字。
func (d *streamDecoder) callID(call *wireFunctionCall) string {
	if call.ID != "" {
		return call.ID
	}
	d.callCounter++
	return codec.SynthToolID(d.messageID, call.Name, d.callCounter)
}

// Finish 补终止事件。本协议的流没有终止标记，读完即结束，
// 所以这些事件必然由这里产出，而不是像别的协议那样可能已在流内出现。
func (d *streamDecoder) Finish() []ir.Event {
	if d.done {
		return nil
	}
	d.done = true
	out := d.closeCurrent()
	delta := ir.Event{Type: ir.EvMessageDelta, StopReason: d.stopReason, Usage: d.usage}
	if delta.StopReason == "" {
		delta.StopReason = ir.StopEndTurn
	}
	if d.sawCall && delta.StopReason == ir.StopEndTurn {
		delta.StopReason = ir.StopToolUse
	}
	return append(out, delta, ir.Event{Type: ir.EvMessageStop})
}

// DecodeResponse 解非流式响应。数据面对上游一律流式，
// 这条路径只在 count_tokens 之类的接口用到。
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
	var notes []string
	maxCandidate := 0
	out := &ir.Response{ID: w.ResponseID, Model: w.ModelVersion, Content: []ir.Block{}}
	if w.UsageMetadata != nil {
		out.Usage = convertUsage(*w.UsageMetadata)
	}
	if w.PromptFeedback != nil && w.PromptFeedback.BlockReason != "" {
		out.StopReason = ir.StopContentFilter
	}

	var calls int
	for _, cand := range w.Candidates {
		if cand.Index != 0 {
			if cand.Index > maxCandidate {
				maxCandidate = cand.Index
			}
			continue
		}
		if cand.FinishMessage != "" {
			notes = append(notes, codec.FinishDetailNote(cand.FinishMessage))
		}
		if cand.FinishReason != "" {
			out.StopReason = convertFinishReason(cand.FinishReason)
		}
		if cand.Content == nil {
			continue
		}
		for _, p := range cand.Content.Parts {
			switch {
			case p.FunctionCall != nil:
				calls++
				id := p.FunctionCall.ID
				if id == "" {
					id = codec.SynthToolID(w.ResponseID, p.FunctionCall.Name, calls)
				}
				out.Content = append(out.Content, ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
					ID: id, Name: p.FunctionCall.Name, Input: string(p.FunctionCall.Args),
				}})
			case p.Thought:
				out.Content = append(out.Content, ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{
					Text: p.Text, Signature: p.ThoughtSignature, SignatureFrom: Name,
				}})
			case p.Text != "":
				out.Content = append(out.Content, ir.Block{Type: ir.BlockText, Text: p.Text})
			}
		}
	}
	if calls > 0 && (out.StopReason == "" || out.StopReason == ir.StopEndTurn) {
		out.StopReason = ir.StopToolUse
	}
	if maxCandidate > 0 {
		notes = append(notes, codec.DroppedCandidatesNote(maxCandidate))
	}
	return out, codec.DedupeNotes(notes), nil
}

func DecodeError(status int, header http.Header, body []byte) *ir.Error {
	var env wireErrorEnvelope
	if err := json.Unmarshal(body, &env); err == nil && env.Error.Message != "" {
		out := codec.WithParam(convertError(status, &env.Error), body)
		// 本协议是四个里唯一把到期时刻放在体内的：google.rpc.RetryInfo。
		// 先填体内的，再让 WithRetryAfter 与头里的取更早者。
		out.RetryAfter = retryInfoAt(env.Error.Details, time.Now())
		return codec.WithRetryAfter(out, header)
	}
	// 不是本协议的错误结构：尽力从任意形状里挖消息，挖不到才回落状态码描述。
	return codec.WithRetryAfter(codec.WithParam(codec.FallbackError(status, body), body), header)
}

// retryInfoAt 从 details 数组里取 RetryInfo.retryDelay 并换成绝对时刻。
//
// 只认 RetryInfo，不认 quotaResetDelay 与 message 里的 "per day" 文本：
// 前者是 Google 的官方 RPC 结构、稳定；后者是文本，上游改文案就静默失效，
// 而失效的方向是「又开始按默认值瞎猜」，无从察觉。
func retryInfoAt(details []wireErrorDetail, now time.Time) time.Time {
	for _, d := range details {
		if d.Type != retryInfoType || d.RetryDelay == "" {
			continue
		}
		delay, err := time.ParseDuration(d.RetryDelay)
		if err != nil || delay <= 0 {
			continue
		}
		at := now.Add(delay)
		// 与头解析走同一个可信性判据：体内的时长同样可能是坏数据。
		if ratelimit.Trustworthy(at, now) {
			return at
		}
	}
	return time.Time{}
}

func convertError(status int, e *wireError) *ir.Error {
	if e == nil {
		return ir.NewError(codec.KindForStatus(status, ""), status, "", "upstream error without detail")
	}
	// 本协议的错误体自带 code，流内错误帧没有 HTTP 状态码可用，
	// 此时以体内的 code 为准。
	if status == 0 {
		status = e.Code
	}
	// 消息位上可能是被字符串化的下游错误体，取出里面的真消息再归类：
	// 上下文超限的判定要看消息文本，读到一串转义引号就判不出来了。
	msg := codec.RefineMessage(e.Message)
	// 状态串（RESOURCE_EXHAUSTED 之类）是流内错误帧唯一的分类依据：
	// 那里没有 HTTP 状态码，不看它限流就会被归成目标故障去冷却。
	return ir.NewError(codec.KindFor(status, e.Status, msg), status, e.Status, msg)
}

func convertUsage(u wireUsage) ir.Usage {
	out := ir.Usage{
		InputTokens: u.PromptTokenCount,
		// 推理消耗不含在 candidatesTokenCount 里，但计费上属于输出，
		// 所以既并进输出总量，又单记一维——与 responses 口径一致。
		OutputTokens:    u.CandidatesTokenCount + u.ThoughtsTokenCount,
		ReasoningTokens: u.ThoughtsTokenCount,
		CacheReadTokens: u.CachedContentTokens,
	}
	// promptTokenCount 含 cachedContentTokenCount（与 Chat Completions 的
	// prompt_tokens 同口径），而 IR 的 InputTokens 定义为不含缓存的新鲜输入，
	// 故减去。上游数字不自洽时钳到 0，不出负数。
	out.InputTokens -= out.CacheReadTokens
	if out.InputTokens < 0 {
		out.InputTokens = 0
	}
	return out
}

func convertFinishReason(s string) ir.StopReason {
	switch s {
	case "STOP":
		return ir.StopEndTurn
	case "MAX_TOKENS":
		return ir.StopMaxTokens
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return ir.StopContentFilter
	// 这四个都是「回答没能正常产出」：OTHER 是本协议的兜底拦截原因，
	// MALFORMED_FUNCTION_CALL 是模型生成的工具调用不合法而被丢弃，
	// LANGUAGE 与 IMAGE_SAFETY 是语言与图片两类内容策略。
	case "OTHER", "MALFORMED_FUNCTION_CALL", "LANGUAGE", "IMAGE_SAFETY":
		return ir.StopContentFilter
	case "FINISH_REASON_UNSPECIFIED", "":
		// 上游没给：留空由聚合层兜底，不能当成被拦截。
		return ""
	default:
		// 未识别的取值按安全侧兜底：把被拦截的回答当正常结束，
		// 客户端会照着不完整的内容继续往下走。
		return ir.StopContentFilter
	}
}
