package gemini

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
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
	// serviceTier 记上游回显的实际执行档位（随 usageMetadata 到达，通常只在
	// 收尾帧带一次）。非空覆盖，经终止 EvMessageDelta 带回，与非流式同口径。
	serviceTier string
	// notes 记录改写说明，走响应侧诊断通道。
	notes []string
	// maxCandidate 是见过的最大候选索引。只记最大值不逐帧记说明：
	// 说明按字符串去重，逐帧生成会让一个流报出好几条不同数字的说明。
	maxCandidate int
	// droppedBadFrames 外层 JSON 都解不开的坏帧计数：结构损坏而非内容损坏，
	// 跳过续流（SSE 以事件边界自同步，坏一帧不污染后续帧），经 Notes() 报出。
	droppedBadFrames int
	// emittedText 累积已下发的正文（不含 thought）：累计式上游每帧重发
	// 全部文本，按前缀比对只放行新增后缀（判据与取舍见 CumulativeTextNote）。
	emittedText strings.Builder
	// cumulativeFrames / rewoundFrames / nulParts 分别计累计帧、重复回退帧
	// 与含 NUL 的文本 part，经 Notes() 报出。
	cumulativeFrames int
	rewoundFrames    int
	nulParts         int
	// droppedUnknownParts 计未建模种类的 part（executableCode 之类），
	// unknownPartKinds 收其字段名，经 Notes() 报出——静默跳过会让客户端
	// 分不出「模型没产这类内容」与「产了被丢了」。
	droppedUnknownParts int
	unknownPartKinds    map[string]bool
	// textIndex/sawText 记最近一个文本块的索引：来源标注（grounding/citation）
	// 在候选级到达，要挂到已开的文本块上（ir.Block.Citations 绑块），聚合器按
	// EvCitation 的 Index 找块累加。没开过文本块时挂不上，计入 droppedCitations。
	textIndex int
	sawText   bool
	// droppedCitations 计无处安放（候选无文本块）的来源标注数，经 Notes() 报出。
	droppedCitations int
	// droppedLogprobs 计携带 logprobsResult 的候选数：逐 token 对数概率没有 IR
	// 槽位，与 chat/responses 解码器同款处置（探测存在性→计数→LogProbsDropNote）。
	droppedLogprobs int
	// droppedModalityDetails 计 usageMetadata 里无法归一进 IR 模态槽位的按模态
	// token 明细条数（VIDEO 两侧、输出侧 IMAGE、缓存与工具用量的模态细分），
	// 经 Notes() 报出——与 droppedLogprobs 同款「给了但 IR 无槽位」处置。
	droppedModalityDetails int
	// droppedSafetyRatings 计候选携带的按类别内容安全评级条数（官方
	// Candidate.safetyRatings，Output only）：IR 无结构化安全评级槽位，与
	// droppedLogprobs 同款处置，经 Notes() 报出。
	droppedSafetyRatings int
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
	if d.droppedBadFrames > 0 {
		notes = append(notes, codec.BadFrameSkipNote(d.droppedBadFrames))
		d.droppedBadFrames = 0
	}
	if d.cumulativeFrames > 0 {
		notes = append(notes, codec.CumulativeTextNote(d.cumulativeFrames))
		d.cumulativeFrames = 0
	}
	if d.rewoundFrames > 0 {
		notes = append(notes, codec.RewoundTextNote(d.rewoundFrames))
		d.rewoundFrames = 0
	}
	if d.nulParts > 0 {
		notes = append(notes, codec.NulTextStripNote(d.nulParts))
		d.nulParts = 0
	}
	if d.droppedUnknownParts > 0 {
		kinds := make([]string, 0, len(d.unknownPartKinds))
		for k := range d.unknownPartKinds {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		notes = append(notes, codec.DroppedUnknownPartsNote(kinds, d.droppedUnknownParts))
		d.droppedUnknownParts = 0
		d.unknownPartKinds = nil
	}
	if d.droppedCitations > 0 {
		notes = append(notes, codec.DroppedCitationsNote(d.droppedCitations))
		d.droppedCitations = 0
	}
	if d.droppedLogprobs > 0 {
		notes = append(notes, codec.LogProbsDropNote(d.droppedLogprobs))
		d.droppedLogprobs = 0
	}
	if d.droppedModalityDetails > 0 {
		notes = append(notes, codec.ModalityUsageDropNote(d.droppedModalityDetails))
		d.droppedModalityDetails = 0
	}
	if d.droppedSafetyRatings > 0 {
		notes = append(notes, codec.SafetyRatingsDropNote(d.droppedSafetyRatings))
		d.droppedSafetyRatings = 0
	}
	return codec.DedupeNotes(notes)
}

func (d *streamDecoder) Feed(event, data string) ([]ir.Event, error) {
	out, split, err := codec.FeedWithSplit(event, data, d.feedOne)
	if split {
		d.notes = append(d.notes, codec.MultipleJSONDocsNote)
	}
	// 整行解不开也拆不出多文档：结构上可忽略的坏帧，计数后吞成无事件无错误，
	// 读流循环据此续流而不是终止整流。内容损坏帧不包裹 ErrSkipFrame，照常上抛。
	if errors.Is(err, codec.ErrSkipFrame) {
		d.droppedBadFrames++
		return nil, nil
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
		// 帧外层解不开：交 ClassifyBadFrame 按「有没有完整文档已解出来」分类——
		// 纯垃圾帧包裹 ErrSkipFrame（Feed 计数跳过续流），残缺多文档行返回内容
		// 损坏错误（fail-fast，除非 FeedWithSplit 能干净拆开）。
		return nil, codec.ClassifyBadFrame(err, data)
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
		u, droppedModality := convertUsage(*frame.UsageMetadata)
		d.droppedModalityDetails += droppedModality
		if d.usage == nil {
			d.usage = &u
		} else {
			ir.MergeUsage(d.usage, u)
		}
		if t := normalizeServiceTier(frame.UsageMetadata.ServiceTier); t != "" {
			d.serviceTier = t
		}
	}
	// 整个请求被安全策略拒了：candidates 为空，只能从 promptFeedback 读出原因。
	if frame.PromptFeedback != nil && frame.PromptFeedback.BlockReason != "" {
		d.stopReason = ir.StopContentFilter
		// 停因只说「被内容过滤挡了」，具体哪条策略命中在原文串里，带出。
		d.notes = append(d.notes, codec.BlockReasonNote(frame.PromptFeedback.BlockReason))
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
		// 来源标注随候选到达（grounding 通常在收尾帧带一次）。挂到已开的文本块
		// 上：聚合器按 EvCitation 的 Index 找块累加，块即使已闭合也仍在表里。
		// 没有文本块可挂（纯函数调用候选）时计数经 Notes() 报出，不静默。
		if cits := candidateCitations(cand); len(cits) > 0 {
			if d.sawText {
				out = append(out, ir.Event{Type: ir.EvCitation, Index: d.textIndex, Citations: cits})
			} else {
				d.droppedCitations += len(cits)
			}
		}
		// 逐 token 对数概率：IR 没有槽位，探测存在性后计数报出（与 chat/responses
		// 解码器同款处置）。只认 logprobsResult（主载荷），avgLogprobs 是其摘要。
		if len(cand.LogprobsResult) > 0 && string(cand.LogprobsResult) != "null" {
			d.droppedLogprobs++
		}
		// 按类别内容安全评级：IR 无结构化槽位，探测计数报出（与 logprobsResult
		// 同款）。空数组/缺席 len 为 0，不误计。
		d.droppedSafetyRatings += len(cand.SafetyRatings)
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
			//
			// 签名从 part 上取而不是从 thought part 上取：本协议把工具调用的
			// 推理签名挂在 functionCall part 自身，丢掉它会让下一轮的调用被
			// 上游当成未经签名的推理续写，退化为不带思考上下文的调用。
			events, err := d.emitCall(p.FunctionCall, p.ThoughtSignature)
			if err != nil {
				return nil, err
			}
			out = append(out, events...)

		case p.Thought:
			out = append(out, d.switchTo(ir.BlockThinking)...)
			// 推理正文只做 NUL 清理，不做累计检测：thought 与正文是两条
			// 独立的文本流，共用一个前缀游标会让 thought 的重复前缀
			// 被误判成正文的回退。签名照常透传。
			if text := d.stripNul(p.Text); text != "" {
				out = append(out, ir.Event{Type: ir.EvThinkingDelta, Index: d.current.index, Text: text})
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
			// 先剥 NUL（清完为空则整 part 跳过，不开块），再按前缀比对
			// 把累计式上游重发的全文压成新增后缀。ok=false 表示这帧没有
			// 净增长（纯重复或回退），吞掉不发空增量。
			text := d.stripNul(p.Text)
			if text == "" {
				continue
			}
			delta, ok := d.classifyText(text)
			if !ok {
				continue
			}
			out = append(out, d.switchTo(ir.BlockText)...)
			// 记下文本块索引：随后到达的来源标注（候选级 grounding/citation）
			// 要挂到这个块上。switchTo 后 d.current 必为文本块（本就开着或刚开）。
			d.textIndex = d.current.index
			d.sawText = true
			out = append(out, ir.Event{Type: ir.EvTextDelta, Index: d.current.index, Text: delta})

		case p.InlineData != nil || p.FileData != nil:
			// 模型返回的图片本服务不往下游转：IR 的图片块只用于请求方向，
			// 三个下游协议对响应内图片的表达各不相同且都不通用。
			//
			// 但要出说明：客户端只看到文字时分不出「模型没画」与
			// 「画了被我们丢了」，而这两者的下一步动作完全不同。
			d.notes = append(d.notes, droppedResponseMediaNote(p))

		default:
			// 未建模的 part 种类（executableCode / codeExecutionResult /
			// videoMetadata / 未来新增）：wirePart 没有对应字段，此前落到
			// switch 外被静默跳过。计数并记种类名，经 Notes() 报出。
			if p.hasUnknownContent() {
				d.droppedUnknownParts++
				if d.unknownPartKinds == nil {
					d.unknownPartKinds = map[string]bool{}
				}
				for _, k := range p.unknownKeys {
					d.unknownPartKinds[k] = true
				}
			}
		}
	}
	return out, nil
}

// stripNul 剥掉正文里的 NUL 字节并计数。上游偶尔把 \u0000 交织进文本，
// 它会毒化下游终端与日志解析器。剥完为空表示这帧本就只含 NUL。
func (d *streamDecoder) stripNul(s string) string {
	clean := stripNulText(s)
	if clean != s {
		d.nulParts++
	}
	return clean
}

// stripNulText 是流式与非流式共用的 NUL 剥离：同一处措辞、同一处置，
// 免得一次丢弃在两条路径上说法不同。不含 NUL 时原样返回（不做分配）。
func stripNulText(s string) string {
	if !strings.ContainsRune(s, 0) {
		return s
	}
	return strings.ReplaceAll(s, "\x00", "")
}

// classifyText 把一帧正文压成相对已下发文本的净增量。
//
// 本协议「每帧一个完整响应对象」的模型下，有的上游按累计式发正文——
// 每帧携带从头到当前的全部文本，而不是增量。直接逐帧转发会让客户端看到
// 文本不断重复叠加。这里按前缀比对：
//   - text 以已下发文本为前缀：是累计帧，只放行新增后缀（delta 为空则吞掉）；
//   - 已下发文本以 text 为前缀：是重复或回退帧，增量流无法表达负增长，吞掉；
//   - 其余：当作正常增量帧原样放行。
//
// 已知取舍：真正的增量上游若恰好发来一整段与已下发文本前缀相同的帧，会被
// 误判为累计帧而只放行后缀。误判概率随文本长度指数下降，参考实现同此取舍
// （见 CumulativeTextNote 的 doc 注释）。
func (d *streamDecoder) classifyText(text string) (string, bool) {
	emitted := d.emittedText.String()
	switch {
	case emitted == text:
		// 纯重复帧：一字未增，吞掉。
		d.rewoundFrames++
		return "", false
	case emitted != "" && strings.HasPrefix(text, emitted):
		// 累计帧：带出了新增后缀，只放行后缀。
		d.cumulativeFrames++
		delta := text[len(emitted):]
		d.emittedText.WriteString(delta)
		return delta, delta != ""
	case emitted != "" && strings.HasPrefix(emitted, text):
		// 回退帧：比已下发的短且是其前缀，增量流表达不了负增长，吞掉。
		d.rewoundFrames++
		return "", false
	default:
		d.emittedText.WriteString(text)
		return text, true
	}
}

// droppedResponseMediaNote 是响应内媒体被丢弃的说明。
//
// 流式与非流式共用这一个出处：两处各写一份措辞的症状是同一次丢弃在
// 流式与探测路径上说法不同，而读说明的人会以为那是两件不同的事。
//
// 带上 media type：丢的是图还是音频决定客户端下一步怎么办，
// 只说「有个媒体块丢了」等于没说。
func droppedResponseMediaNote(p wirePart) string {
	media := "unknown"
	switch {
	case p.InlineData != nil && p.InlineData.MimeType != "":
		media = p.InlineData.MimeType
	case p.FileData != nil && p.FileData.MimeType != "":
		media = p.FileData.MimeType
	}
	return "dropped a " + media + " part from the response (" + Name +
		" is the only protocol expressing it and the neutral representation " +
		"carries media only in the request direction)"
}

// candidateCitations 从 gemini 候选的来源标注里抽出可移植的 URL 引用，按 URL
// 去重。citationMetadata.citationSources 与 groundingMetadata 的 web /
// retrievedContext 块都带 URI，合并成 ir.Citation——chat 的 annotations、
// anthropic 的 citations 填的是同一个槽位，gemini 上游此前整路静默丢弃。
//
// 只保全来源身份（URL+标题+被引原文），不携带数字范围：gemini 的
// startIndex/endIndex 是相对候选**全文**的**字节**偏移，而 ir.Citation.Start/End
// 是单个文本块内的 **rune** 偏移，且流式正文按累计/回退帧下发（见
// classifyText），字节→rune 换算在这条路径上不可靠——错位的引用会把来源挂到
// 错误的文字段上，比不带范围更糟。范围缺席时 HasRange 为假，下游编码器照常
// 按「只有 URL 的引用」处置，Portable 仍为真，来源身份不丢。
func candidateCitations(c wireCandidate) []ir.Citation {
	var out []ir.Citation
	seen := map[string]bool{}
	add := func(url, title, cited string) {
		if url == "" || seen[url] {
			return
		}
		seen[url] = true
		out = append(out, ir.Citation{URL: url, Title: title, CitedText: cited})
	}
	if c.CitationMetadata != nil {
		for _, s := range c.CitationMetadata.CitationSources {
			add(s.URI, "", "")
		}
	}
	if c.GroundingMetadata != nil {
		for _, ch := range c.GroundingMetadata.GroundingChunks {
			if ch.Web != nil {
				add(ch.Web.URI, ch.Web.Title, "")
			}
			if ch.RetrievedContext != nil {
				add(ch.RetrievedContext.URI, ch.RetrievedContext.Title, ch.RetrievedContext.Text)
			}
		}
	}
	// url_context 工具的检索确认：只有 SUCCESS（或 UNSPECIFIED/空，即上游
	// 没给状态时默认成功）的 URL 才是模型实际用到的来源，映进 Citation；
	// ERROR/PAYWALL/UNSAFE 的 URL 模型没读到内容，不算引用、不采集。
	if c.UrlContextMetadata != nil {
		for _, um := range c.UrlContextMetadata.UrlMetadata {
			switch um.UrlRetrievalStatus {
			case "", "UNSPECIFIED", "SUCCESS":
				add(um.RetrievedUrl, "", "")
			}
		}
	}
	return out
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
func (d *streamDecoder) emitCall(call *wireFunctionCall, signature string) ([]ir.Event, error) {
	out := d.closeCurrent()
	d.sawCall = true

	index := d.nextIndex
	d.nextIndex++
	// 参数原样进 IR：残缺/非对象也不清空——{} 会让这次调用看起来是
	// 一次合法的无参调用，截断被无声吞掉。IR 槽位是字符串形态装得下
	// 原文，聚合器的 IncompleteTools 会把残缺值判出来，出站编码时
	// 再按目标协议的槽位形态处置（对象槽位挪 ir.RawArgsKey）。
	args := string(call.Args)
	use := &ir.ToolUse{ID: d.callID(call), Name: call.Name}
	if signature != "" {
		use.Signature = signature
		use.SignatureFrom = Name
	}
	out = append(out,
		ir.Event{Type: ir.EvBlockStart, Index: index, Block: &ir.Block{
			Type:    ir.BlockToolUse,
			ToolUse: use,
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
	delta := ir.Event{Type: ir.EvMessageDelta, StopReason: d.stopReason, Usage: d.usage, ServiceTier: d.serviceTier}
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
	nulParts := 0
	unknownParts := 0
	unknownKinds := map[string]bool{}
	out := &ir.Response{ID: w.ResponseID, Model: w.ModelVersion, Content: []ir.Block{}}
	if w.UsageMetadata != nil {
		var droppedModality int
		out.Usage, droppedModality = convertUsage(*w.UsageMetadata)
		if droppedModality > 0 {
			notes = append(notes, codec.ModalityUsageDropNote(droppedModality))
		}
		// 档位回声随 usageMetadata 到达：此前不读→gemini 上游的实际执行档位
		// 静默丢失、客户端看不到也无注记。归一后原值进 IR，跨族由入站编码器
		// 的 MapServiceTierEcho/TierEchoDropNote 处理。
		out.ServiceTier = normalizeServiceTier(w.UsageMetadata.ServiceTier)
	}
	if w.PromptFeedback != nil && w.PromptFeedback.BlockReason != "" {
		out.StopReason = ir.StopContentFilter
		// 与流式同一处置：具体阻断原因串带出，不只留一个 content_filter 停因。
		notes = append(notes, codec.BlockReasonNote(w.PromptFeedback.BlockReason))
	}

	var calls int
	var droppedCitations int
	var logprobs int
	var safetyRatings int
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
		// 来源标注（grounding/citation metadata）在候选级到达，映射成 ir.Citation
		// 后挂到本候选的文本块上；候选没有正文块可挂时计入丢弃、经注记报出。
		cits := candidateCitations(cand)
		if cand.Content == nil {
			droppedCitations += len(cits)
			continue
		}
		textBlockIdx := -1
		for _, p := range cand.Content.Parts {
			switch {
			case p.FunctionCall != nil:
				calls++
				id := p.FunctionCall.ID
				if id == "" {
					id = codec.SynthToolID(w.ResponseID, p.FunctionCall.Name, calls)
				}
				use := &ir.ToolUse{
					ID: id, Name: p.FunctionCall.Name, Input: string(p.FunctionCall.Args),
				}
				// 与流式同一处：签名挂在 functionCall part 自身。
				if p.ThoughtSignature != "" {
					use.Signature = p.ThoughtSignature
					use.SignatureFrom = Name
				}
				out.Content = append(out.Content, ir.Block{Type: ir.BlockToolUse, ToolUse: use})
			case p.Thought:
				text := stripNulText(p.Text)
				if strings.ContainsRune(p.Text, 0) {
					nulParts++
				}
				out.Content = append(out.Content, ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{
					Text: text, Signature: p.ThoughtSignature, SignatureFrom: Name,
				}})
			case p.Text != "":
				text := stripNulText(p.Text)
				if strings.ContainsRune(p.Text, 0) {
					nulParts++
				}
				if text == "" {
					continue
				}
				if textBlockIdx < 0 {
					textBlockIdx = len(out.Content)
				}
				out.Content = append(out.Content, ir.Block{Type: ir.BlockText, Text: text})
			case p.InlineData != nil || p.FileData != nil:
				// 与流式同一处置、同一措辞。这个分支此前不存在，媒体 part
				// 落到 switch 外面被静默跳过——连「丢了」都不在代码里。
				notes = append(notes, droppedResponseMediaNote(p))
			default:
				// 未建模的 part 种类：与流式同一处置，计数+记种类名后报出。
				if p.hasUnknownContent() {
					unknownParts++
					for _, k := range p.unknownKeys {
						unknownKinds[k] = true
					}
				}
			}
		}
		if len(cits) > 0 {
			if textBlockIdx >= 0 {
				out.Content[textBlockIdx].Citations = append(out.Content[textBlockIdx].Citations, cits...)
			} else {
				droppedCitations += len(cits)
			}
		}
		if len(cand.LogprobsResult) > 0 && string(cand.LogprobsResult) != "null" {
			logprobs++
		}
		// 按类别内容安全评级：IR 无结构化槽位，探测计数报出（与流式同判据）。
		safetyRatings += len(cand.SafetyRatings)
	}
	if calls > 0 && (out.StopReason == "" || out.StopReason == ir.StopEndTurn) {
		out.StopReason = ir.StopToolUse
	}
	if maxCandidate > 0 {
		notes = append(notes, codec.DroppedCandidatesNote(maxCandidate))
	}
	if nulParts > 0 {
		notes = append(notes, codec.NulTextStripNote(nulParts))
	}
	if unknownParts > 0 {
		kinds := make([]string, 0, len(unknownKinds))
		for k := range unknownKinds {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		notes = append(notes, codec.DroppedUnknownPartsNote(kinds, unknownParts))
	}
	if droppedCitations > 0 {
		notes = append(notes, codec.DroppedCitationsNote(droppedCitations))
	}
	if logprobs > 0 {
		notes = append(notes, codec.LogProbsDropNote(logprobs))
	}
	if safetyRatings > 0 {
		notes = append(notes, codec.SafetyRatingsDropNote(safetyRatings))
	}
	return out, codec.DedupeNotes(notes), nil
}

func DecodeError(status int, header http.Header, body []byte) *ir.Error {
	var env wireErrorEnvelope
	// 字段类型不匹配不算「什么都没解到」：外层 body 已是合法 JSON，Go 的
	// 解码器记下类型错误后仍会解完其余键——message 写成数字时，解得好的
	// status 串与 RetryInfo 不能跟着一起丢：前者是归因，后者是退避依据。
	_ = json.Unmarshal(body, &env)
	if env.Error.Message == "" && env.Error.Status == "" && len(env.Error.Details) == 0 {
		// 不是本协议的错误结构：尽力从任意形状里挖消息，挖不到才回落状态码描述。
		return codec.WithRetryAfter(codec.WithParam(codec.FallbackError(status, body), body), header)
	}
	var out *ir.Error
	if env.Error.Message != "" {
		out = convertError(status, &env.Error)
	} else {
		out = codec.SalvagedError(status, body, env.Error.Status)
	}
	// 本协议是四个里唯一把到期时刻放在体内的：google.rpc.RetryInfo。
	// 先填体内的，再让 WithRetryAfter 与头里的取更早者。
	out.RetryAfter = retryInfoAt(env.Error.Details, time.Now())
	return codec.WithRetryAfter(codec.WithParam(out, body), header)
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

// normalizeServiceTier 把 gemini 的档位回声归一进 IR 的原值口径。
// unspecified 是 enum 的零值（官方注「Default service tier, which is standard」），
// 与「上游没给这个字段」不可分，且不是任何客户端能识别的标准枚举值，故归零为
// 缺席，避免把陌生值原样透传给客户端；standard/flex/priority 原值保留，跨族映射
// 与「装不下就丢弃+注记」交由既有 codec.MapServiceTierEcho / TierEchoDropNote
// 处理（gemini 只出站、无面向客户端的响应编码器，故本族不再二次映射）。
func normalizeServiceTier(s string) string {
	if s == "unspecified" {
		return ""
	}
	return s
}

func convertUsage(u wireUsage) (ir.Usage, int) {
	out := ir.Usage{
		InputTokens: u.PromptTokenCount,
		// 推理消耗与工具调用消耗都不含在 candidatesTokenCount 里，但计费上
		// 都属于输出，所以并进输出总量才与账单一致。推理另单记一维
		// （ReasoningTokens）与 responses 口径对齐；工具调用消耗无对应细分维，
		// 只并入总量，不新开一维（它是对同批输出 token 的再细分，不过进程边界）。
		OutputTokens:    u.CandidatesTokenCount + u.ThoughtsTokenCount + u.ToolUsePromptTokenCount,
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
	// 模态明细：把 gemini 按 TEXT/IMAGE/AUDIO/VIDEO 拆分的 token 归一进 IR 既有
	// 的模态槽位（与 chat prompt_tokens_details.image_tokens/text_tokens 同维，
	// 轮次41 已建槽位与持久化链）。IR 无对应槽位的（VIDEO 两侧、输出侧 IMAGE、
	// 缓存与工具用量的模态细分）计入第二返回值，由调用方经注记报出，不静默丢弃。
	var unmappable int
	for _, m := range u.PromptTokensDetails {
		switch m.Modality {
		case "TEXT":
			out.PromptTextTokens += m.TokenCount
		case "IMAGE":
			out.PromptImageTokens += m.TokenCount
		case "AUDIO":
			out.PromptAudioTokens += m.TokenCount
		default: // VIDEO / MODALITY_UNSPECIFIED / 未识别：IR 无输入视频槽位。
			unmappable++
		}
	}
	// 输出侧：Gemini API 用 responseTokensDetails、Vertex 用 candidatesTokensDetails
	// 指同一份输出明细，同一响应只会给其一，两者都读。
	for _, m := range u.CandidatesTokensDetails {
		unmappable += mapOutputModality(&out, m)
	}
	for _, m := range u.ResponseTokensDetails {
		unmappable += mapOutputModality(&out, m)
	}
	// 缓存与工具用量的模态细分：IR 只有缓存读取总量与并入输出的工具消耗总量，
	// 没有按模态的缓存/工具细分槽位，整组计数报出。
	unmappable += len(u.CacheTokensDetails) + len(u.ToolUsePromptTokensDetails)
	return out, unmappable
}

// mapOutputModality 把一条输出侧模态明细归一进 IR，返回无法归一的条数（0 或 1）。
// 输出侧 IR 只有文本与音频槽位；图片/视频输出（如 gemini 图像生成）无处安放。
func mapOutputModality(out *ir.Usage, m wireModalityTokenCount) int {
	switch m.Modality {
	case "TEXT":
		out.CompletionTextTokens += m.TokenCount
		return 0
	case "AUDIO":
		out.CompletionAudioTokens += m.TokenCount
		return 0
	default: // IMAGE / VIDEO / MODALITY_UNSPECIFIED / 未识别。
		return 1
	}
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
