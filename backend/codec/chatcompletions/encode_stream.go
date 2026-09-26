package chatcompletions

import (
	"encoding/json"
	"sort"
	"strings"
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
	// toolPending 记录已开启但还没见过任何 arguments 增量的工具调用。
	// 客户端只从 delta 拼参数，零增量（无参工具是常态）会拼出 ""，
	// json.loads 直接崩，关块时要补一个 "{}"。
	toolPending map[int]bool
	// toolArgs 累积各工具调用已下发的 arguments 增量。增量一旦发出就
	// 不可改写，畸形参数（多为 max_tokens 截断）只能原样透传，关块时
	// 校验累积值并计数，由 Notes 报出——否则客户端会把一次参数损坏的
	// 调用当正常完成存进历史。
	toolArgs map[int][]byte
	// text 累积各块已下发的正文。标注到达时要在这份文本上解析范围——
	// 客户端手里的正文就是它，偏移量对不上等于高亮指错位置。
	text map[int]string
	// badToolArgs 是关块时判定畸形的工具调用数，Notes() 报出。
	badToolArgs int
	stopReason  ir.StopReason
	// serviceTier 是已映射待回显的执行档位（codec.MapServiceTierEcho），
	// 一旦收到就挂在此后的每个 chunk 上——chat 的晚到回显补得上。
	// 不回填已发出的帧——发出去的改不了。
	serviceTier string
	// droppedTier 没能回显的档位原值（越集，如 anthropic 的 batch、
	// responses 的 ultrafast），Notes() 收尾时报出。
	droppedTier string
	// fingerprint 后端配置指纹：与档位同样的回显规律，一旦收到就挂在
	// 此后的每个 chunk 上。chat 专属维度，异族来源给不出，跨族丢弃
	// 不报——排障信号，不是计费或内容维度。
	fingerprint string
	// usage 跨帧累积：input 与 output 可能来自不同的 IR 事件。
	usage ir.Usage
	// droppedImages / droppedFiles 被跳过的模型产出附件块数（image /
	// audio+document+file）：delta.message 只有 content / refusal /
	// tool_calls / 思考几个槽位，附件没有对应形态，整块跳过。
	// 计数在 Notes() 收尾时报出。
	droppedImages int
	droppedFiles  int
	// droppedServerCalls / droppedServerResults 被跳过的托管工具块数：
	// 本族编码器没有为 Anthropic 的 server_tool_use /
	// web_search_tool_result 输出任何对应形态，同上。两种块型分开计数，
	// 注记只渲染非零的那半。
	droppedServerCalls   int
	droppedServerResults int
	// droppedCites 带不出本族的引用条数（Anthropic 的文档类引用没有 URL，
	// 而本族的标注槽位以 URL 为来源身份），同上。
	droppedCites int
	// droppedContainer 容器回显（anthropic 专属维度）被丢标记：本协议响应
	// 没有 container 槽位。首帧或收尾帧任一带到即置位，Notes() 报一次。
	droppedContainer bool
	// droppedAudio 完整音频输出（chat 非流式响应投影而来）被丢标记：
	// SSE delta 没有官方音频槽位，本体、转写与回放 id 一起消失。
	droppedAudio bool
	// droppedUploads 被跳过的容器文件引用块（container_upload）数：本协议
	// 没有 file_id 槽位，整块跳过，Notes() 收尾时报出。
	droppedUploads int
	// droppedCacheDetails 缓存写入 TTL 明细（anthropic 专属维度）被丢标记：
	// 本协议 usage 没有 5m/1h 细分槽位，写入总量仍完整保留。
	droppedCacheDetails bool
	// notes 是响应侧丢弃说明，累加后由 Notes 去重排序交出。
	notes []string
	// suppressUsageFrame 为真表示客户端明确说了不要那一帧单独的 usage
	// （stream_options 给了但 include_usage 是 false）。
	//
	// 只压这一帧，finish_reason 的空 delta 与 [DONE] 照发：那两个是协议
	// 终止形状，压掉会让客户端等一个永不到来的结束。也不记有损——
	// 这是照客户端的要求执行，不是丢了它要的东西。
	suppressUsageFrame bool
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
	if e.droppedCites > 0 {
		notes = append(notes, codec.CitationDropNote(e.droppedCites))
	}
	if e.badToolArgs > 0 {
		notes = append(notes, ir.RawArgsPassNote(e.badToolArgs))
	}
	if e.droppedContainer {
		notes = append(notes, codec.ContainerDropNote())
	}
	if e.droppedAudio {
		notes = append(notes, codec.AudioOutputDropNote())
	}
	if e.droppedUploads > 0 {
		notes = append(notes, codec.ContainerUploadDropNote(e.droppedUploads))
	}
	if e.droppedCacheDetails {
		notes = append(notes, codec.CacheCreationDetailsDropNote())
	}
	// usage 细分维度与 TTL 明细同口径门控：客户端没 opt-in 时 usage 帧
	// 压根没发，细分也就无所谓「没能交付」。
	if !e.suppressUsageFrame {
		if dims := codec.UsageDropDims(&e.usage, Name); len(dims) > 0 {
			notes = append(notes, codec.UsageDetailDropNote(dims))
			e.usage = ir.Usage{}
		}
	}
	if e.droppedTier != "" {
		notes = append(notes, codec.TierEchoDropNote(e.droppedTier))
		e.droppedTier = ""
	}
	return codec.DedupeNotes(notes)
}

// mapTier 映射档位回显：值集装不下的（anthropic 的 batch、responses 的
// ultrafast）丢弃，Notes() 报出。重复到达时先到先得，不覆盖不重复报。
func (e *streamEncoder) mapTier(raw string) {
	if raw == "" || e.serviceTier != "" || e.droppedTier != "" {
		return
	}
	if tier, ok := codec.MapServiceTierEcho(raw, Name); ok {
		e.serviceTier = tier
	} else {
		e.droppedTier = raw
	}
}

func newStreamEncoder() *streamEncoder {
	return &streamEncoder{
		blockKind:      map[int]ir.BlockType{},
		toolIndex:      map[int]int{},
		sentToolHeader: map[int]bool{},
		toolPending:    map[int]bool{},
		toolArgs:       map[int][]byte{},
		text:           map[int]string{},
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
		e.mapTier(ev.ServiceTier)
		if ev.SystemFingerprint != "" && e.fingerprint == "" {
			e.fingerprint = ev.SystemFingerprint
		}
		if ev.Container != nil {
			e.droppedContainer = true
		}
		if ev.Audio != nil {
			e.droppedAudio = true
		}
		// 上游给过创建时间就原值逐帧回写（覆盖构造时的本地钟）；没给才用本地钟。
		if ev.Created != 0 {
			e.created = ev.Created
		}
		// Anthropic 上游在这一帧给 input_tokens，而本协议只有末尾一帧 usage，
		// 不在这里收下就永远丢了。
		if ev.Usage != nil {
			ir.MergeUsage(&e.usage, *ev.Usage)
			if ev.Usage.CacheWriteDetailsKnown {
				e.droppedCacheDetails = true
			}
		}
		// 本协议的首帧就是一个带 role 的 delta，没有独立的消息头帧。
		return e.chunk(wireMessage{Role: roleAssistant}, "")

	case ir.EvBlockStart:
		kind := ir.BlockText
		if ev.Block != nil {
			kind = ev.Block.Type
		}
		e.blockKind[ev.Index] = kind
		switch kind {
		case ir.BlockImage:
			// 模型产出的附件没有本族增量形态：整块跳过但计数，Notes() 报出。
			e.droppedImages++
			return nil, nil
		case ir.BlockAudio, ir.BlockDocument, ir.BlockFile:
			e.droppedFiles++
			return nil, nil
		case ir.BlockServerToolUse:
			// 服务端托管工具块没有本族形态：整块跳过但计数，Notes() 报出。
			// kind 已记进 blockKind，该索引后续的查询串增量一律丢弃——
			// 否则会被 toolSlot 编成一场客户端从未发起的伪工具调用。
			e.droppedServerCalls++
			return nil, nil
		case ir.BlockWebSearchToolResult:
			// 搜回来的页面同理：跳过但计数。
			e.droppedServerResults++
			return nil, nil
		case ir.BlockContainerUpload:
			// 容器文件引用（file_id）没有本族形态：整块跳过但计数，Notes() 报出。
			// kind 已记进 blockKind，skipDelta 据此挡住该索引的任何后续增量，
			// 不让 file_id 或空正文被编成凭空的文本/工具调用。
			e.droppedUploads++
			return nil, nil
		}
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
		e.toolPending[ev.Index] = true
		e.toolArgs[ev.Index] = nil
		return e.chunk(wireMessage{ToolCalls: []wireToolCall{call}}, "")

	case ir.EvTextDelta:
		if e.skipDelta(ev.Index) {
			return nil, nil
		}
		if e.blockKind[ev.Index] == ir.BlockRefusal {
			// 拒绝块的增量走 delta.refusal 而不是 delta.content：
			// 写进 content 会被客户端渲染成普通回答，拒绝标记就丢了。
			return e.chunk(wireMessage{Refusal: ev.Text}, "")
		}
		// 先累积再编帧：随后的标注要用这份文本解析偏移量。
		e.text[ev.Index] += ev.Text
		content, err := json.Marshal(ev.Text)
		if err != nil {
			return nil, err
		}
		return e.chunk(wireMessage{Content: content}, "")

	case ir.EvCitation:
		// 标注编成 delta.annotations 一帧带全：本协议的标注不是逐条增量，
		// 客户端按到达顺序累积。无法解析范围的条目照样发（本协议允许
		// 无范围标注），全空才不发。没有 URL 的（Anthropic 文档类引用）
		// 装不进 url_citation，计数后由 Notes() 报出。
		e.droppedCites += codec.CountNonPortableCitations(ev.Citations)
		as := encodeAnnotations(e.text[ev.Index], ev.Citations)
		if len(as) == 0 {
			return nil, nil
		}
		return e.chunk(wireMessage{Annotations: as}, "")

	case ir.EvThinkingDelta:
		if e.skipDelta(ev.Index) {
			return nil, nil
		}
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
		if e.skipDelta(ev.Index) {
			return nil, nil
		}
		n := e.toolSlot(ev.Index)
		e.toolArgs[ev.Index] = append(e.toolArgs[ev.Index], ev.Text...)
		call := wireToolCall{Index: &n, Function: wireFunctionCall{Arguments: ev.Text}}
		if !e.sentToolHeader[ev.Index] {
			// 上游漏发块开启帧时补上 type，否则客户端不知道这是函数调用。
			call.Type = "function"
			e.sentToolHeader[ev.Index] = true
		}
		// 见过真实增量的调用不再是零增量。
		delete(e.toolPending, ev.Index)
		return e.chunk(wireMessage{ToolCalls: []wireToolCall{call}}, "")

	case ir.EvBlockStop:
		// 本协议无块边界概念，块闭合本身无需表达；但零增量的工具调用
		// 要在此补一个 "{}" 增量，依据见 toolPending 的注释。
		e.finishToolArgs(ev.Index)
		if e.toolPending[ev.Index] {
			delete(e.toolPending, ev.Index)
			n := e.toolSlot(ev.Index)
			return e.chunk(wireMessage{ToolCalls: []wireToolCall{
				{Index: &n, Function: wireFunctionCall{Arguments: "{}"}},
			}}, "")
		}
		return nil, nil

	case ir.EvMessageDelta:
		if ev.StopReason != "" {
			e.stopReason = ev.StopReason
		}
		// 上游可能只在收尾帧给档位（非流式响应投影成事件时就是这样）。
		e.mapTier(ev.ServiceTier)
		// 指纹同理：chat 解码器把它攒到收尾帧交付，晚到的也补得上。
		if ev.SystemFingerprint != "" && e.fingerprint == "" {
			e.fingerprint = ev.SystemFingerprint
		}
		if ev.Container != nil {
			e.droppedContainer = true
		}
		if ev.Usage != nil {
			ir.MergeUsage(&e.usage, *ev.Usage)
			if ev.Usage.CacheWriteDetailsKnown {
				e.droppedCacheDetails = true
			}
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
	// 关块帧没来就断流的工具调用在这里兜底校验：参数已发出，畸形的
	// 只能计数报出。错误收尾也要报——错误之前下发的参数同样不可执行。
	for idx := range e.toolArgs {
		e.finishToolArgs(idx)
	}
	// 错误帧已自带 [DONE]，这里再发一套会让客户端读到两个终止。
	if e.stopped || e.errored {
		return nil, nil
	}
	e.stopped = true

	// 流被掐断时块闭合帧不会来，零增量的工具调用在这里补 "{}"，
	// 与 EvBlockStop 分支同一口径。
	var frames [][]byte
	if len(e.toolPending) > 0 {
		idxs := make([]int, 0, len(e.toolPending))
		for idx := range e.toolPending {
			idxs = append(idxs, idx)
		}
		sort.Ints(idxs)
		for _, idx := range idxs {
			n := e.toolSlot(idx)
			frame, err := e.chunk(wireMessage{ToolCalls: []wireToolCall{
				{Index: &n, Function: wireFunctionCall{Arguments: "{}"}},
			}}, "")
			if err != nil {
				return nil, err
			}
			frames = append(frames, frame...)
		}
		e.toolPending = map[int]bool{}
	}

	out, err := e.chunk(wireMessage{}, renderFinishReason(e.stopReason))
	if err != nil {
		return nil, err
	}
	out = append(frames, out...)
	if !e.suppressUsageFrame {
		u := renderUsage(e.usage)
		frame, err := e.marshal(wireResponse{
			ID: e.messageID(), Object: chunkObject, Created: e.created,
			Model: e.model, Choices: []wireChoice{}, Usage: &u,
			ServiceTier: e.serviceTier, SystemFingerprint: e.fingerprint,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, frame)
	}
	return append(out, codec.EncodeFrame("", []byte(doneSentinel))), nil
}

func (e *streamEncoder) Finish() [][]byte {
	out, err := e.finish()
	if err != nil {
		return nil
	}
	return out
}

// skipDelta 报告某个块索引是否属于要在增量通道上整块丢弃的块型：服务端托管
// 工具块与容器文件引用块。那两类块在 EvBlockStart 已被整块跳过，但托管工具的
// 查询串仍会经 EvToolInput 通道续传，不挡住就会被 toolSlot 编成一场客户端
// 从未发起的伪工具调用；容器文件引用块本无增量，这里一并挡住是防御性的。
func (e *streamEncoder) skipDelta(blockIndex int) bool {
	kind := e.blockKind[blockIndex]
	return kind.IsServerTool() || kind == ir.BlockContainerUpload
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

// finishToolArgs 在关块（或断流兜底）时校验累积的 arguments。增量已发出、
// 改写不了，畸形的只能计数报出（RawArgsPassNote），让客户端知道这次调用的
// 参数不能安全执行，而不是看起来以空对象正常完成。
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

func (e *streamEncoder) chunk(delta wireMessage, finish string) ([][]byte, error) {
	frame, err := e.marshal(wireResponse{
		ID: e.messageID(), Object: chunkObject, Created: e.created, Model: e.model,
		Choices:     []wireChoice{{Index: 0, Delta: &delta, FinishReason: finish}},
		ServiceTier: e.serviceTier, SystemFingerprint: e.fingerprint,
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
	// 模型音频输出是本协议非流式响应的原生维度：四键照原样写回，
	// 客户端靠 id 在下一轮回传、靠 data 播放、靠 transcript 展示。
	if resp.Audio != nil {
		raw, err := json.Marshal(audioOutput{
			ID: resp.Audio.ID, Data: resp.Audio.Data,
			ExpiresAt: resp.Audio.ExpiresAt, Transcript: resp.Audio.Transcript,
		})
		if err != nil {
			return nil, err
		}
		msg.Audio = raw
	}
	var (
		text  []ir.Block
		calls []wireToolCall
		// annotations 的偏移量相对整条消息 content 的拼接文本，
		// 块内坐标逐块平移后统一编码。
		cites    []ir.Citation
		citeText strings.Builder
	)
	for _, b := range resp.Content {
		switch b.Type {
		case ir.BlockThinking:
			if b.Thinking != nil {
				msg.ReasoningContent += b.Thinking.Text
			}
		case ir.BlockRefusal:
			// 拒绝正文回本族专属槽位：落进 content 会被客户端渲染成普通
			// 回答，「模型拒绝了」这个事实就丢了。
			msg.Refusal += b.Text
		case ir.BlockToolUse:
			if b.ToolUse == nil {
				continue
			}
			n := len(calls)
			if b.ToolUse.Kind == ir.ToolCustom {
				// 本族原生形态（type=custom，custom{name,input}），不再投影成
				// 函数。官方 chat 协议认 custom 调用；投影会给自由文本入参
				// 套一层 {"input":…} 的 JSON 壳。
				calls = append(calls, wireToolCall{
					Index: &n, ID: b.ToolUse.ID, Type: "custom",
					Custom: &wireCustomCall{Name: b.ToolUse.Name, Input: b.ToolUse.InputText},
				})
				continue
			}
			// arguments 是字符串槽位：畸形原文照转义嵌入，响应体不会因此
			// 非法。不清空成 {}——那会让客户端把参数损坏的调用当无参调用
			// 存进历史，损耗由 EncodeResponseLossy 报出。
			args := b.ToolUse.Input
			calls = append(calls, wireToolCall{
				Index: &n, ID: b.ToolUse.ID, Type: "function",
				Function: wireFunctionCall{Name: b.ToolUse.Name, Arguments: args},
			})
		case ir.BlockImage, ir.BlockAudio, ir.BlockDocument, ir.BlockFile:
			// 助手消息的 content 只有文本/拒绝两种合法形态：附件块落进
			// default 会被 encodeContent 编成用户侧才有的 image_url part，
			// 那是非法的助手消息形状。跳过，损耗由 EncodeResponseLossy 报出。
			continue
		case ir.BlockServerToolUse, ir.BlockWebSearchToolResult:
			// 服务端托管工具块没有本族输出形态：整块跳过，落进 default
			// 会被当正文编成一段凭空的查询串或搜索结果。
			// 跳过，损耗由 EncodeResponseLossy 经 CountResponseServerTools 报出。
			continue
		case ir.BlockContainerUpload:
			// 容器文件引用块没有本族输出形态：整块跳过，落进 default 会被当
			// 正文编成一个空 content part（file_id 不会泄漏，但会凭空多一段空文本）。
			// 跳过，损耗由 EncodeResponseLossy 经 CountResponseContainerUploads 报出。
			continue
		default:
			if b.Type == ir.BlockText {
				cites = append(cites, shiftCitations(b.Citations, citeText.String(), b.Text)...)
				citeText.WriteString(b.Text)
			}
			text = append(text, b)
		}
	}
	content, err := encodeContent(text)
	if err != nil {
		return nil, err
	}
	msg.Content = content
	msg.ToolCalls = calls
	msg.Annotations = encodeAnnotations(citeText.String(), cites)

	u := renderUsage(resp.Usage)
	// 上游给过创建时间就原值回写；没给才回退本地钟——同族往返不能把上游的
	// 真实 created 换成代理本地时间（客户端按它做幂等/排序）。
	created := resp.Created
	if created == 0 {
		created = time.Now().Unix()
	}
	out := wireResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: created,
		Model:   resp.Model,
		Choices: []wireChoice{{
			Index: 0, Message: &msg, FinishReason: renderFinishReason(resp.StopReason),
		}},
		Usage: &u,
	}
	// 实际执行档位回显：跨族按回显值集翻译，装不下的（anthropic 的
	// batch、responses 的 ultrafast）丢弃，由 DescribeResponseTierLoss 报出。
	if tier, ok := codec.MapServiceTierEcho(resp.ServiceTier, Name); ok {
		out.ServiceTier = tier
	}
	// 指纹原样回写：chat 专属维度，异族来源恒为空串，omitempty 自然不带。
	out.SystemFingerprint = resp.SystemFingerprint
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
		Param:   err.Param,
		Type:    errorTypeForKind(err.Kind),
		Code:    code,
	}}
}

// errorTypeForKind 用 OpenAI 的错误类型名，让客户端 SDK 能按自己的分类处理。
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
	default:
		return "server_error"
	}
}
