package ir

import (
	"encoding/json"
	"strconv"
	"strings"
)

// maxContentBytes 是一次响应累计内容字节上限，与非 SSE 整份响应上限
// （pipeline 的 maxWholeResponseBytes）同值：同一份内容换个传输形态不该拿到
// 更宽松的预算。单帧上限（codec 的 maxFrameBytes）只管一帧，乘以无限帧数
// 等于没有上限。
const maxContentBytes = 32 << 20

// maxBlocks 是一次响应的内容块数上限。四个协议的真实块数都是「个」到
// 「十几个」的量级，4096 是三个数量级的余量；而无界会让上游一个索引跳号的
// bug 变成本服务的内存增长——块索引是上游给的，本服务不校验它。
const maxBlocks = 4096

// acc 是一个块的累积器。
//
// 不挂在 Block 上：Block 要原样序列化进下一轮请求，不该携带解码过程的状态
// （与 sawInput 同一条理由）。
//
// 用 Builder 而不是往 Block 的字符串字段上 += ：后者每帧一次全量重分配，
// 实测 160000 帧（10 MiB 文本）耗时 3 分钟、累计分配 763 GiB，而这条路径
// 每个请求都走——流式也要靠聚合器算 usage 与判残缺。
type acc struct {
	text     strings.Builder
	thinking strings.Builder
	sig      strings.Builder
	input    strings.Builder
}

// Aggregator 把事件流聚合回 Response。
//
// 对上游一律发流式请求，客户端要非流式时用它把事件收拢成一次性响应。
// 这样上游只有一条代码路径，非流式不必单独实现一遍解码。
type Aggregator struct {
	resp   Response
	blocks map[int]*Block
	// order 记录块首次出现的顺序。上游的块索引不保证连续或有序
	// （responses 的 output_index 就可能跳号），不能靠索引排序。
	order []int
	// sawInput 记录哪些块收到过工具入参增量，用于区分「上游本就没给入参」
	// 与「入参发到一半断流」。记在这里而不是 Block 上：Block 要原样序列化
	// 进下一轮请求，不该携带解码过程的状态。
	sawInput map[int]bool
	// closed 记录收到过 block_stop 的块。没收到就是断流时块还开着，
	// 这类块的内容按协议无法判定完整。
	closed map[int]bool
	// accs 与 blocks 同键，持有各块内容的 Builder。
	accs map[int]*acc
	// contentBytes 是累计内容字节。独立字段累加而不是遍历所有 Builder 求和：
	// 后者是每帧一次 O(块数) 的遍历，把刚消掉的二次复杂度又装回来。
	contentBytes int
	// overflow 非空表示撞了某道闸门，内容里是闸门名与它的数值。
	overflow string
}

// Overflow 报告是否撞了累积闸门，返回值非空时是可直接进错误消息的说明。
//
// 做成查询而不是让 Add 返回错误：bridge 在补「按 max_tokens 截断」的终止事件时
// 也会调 Add，那是本服务自造的事件、不该被闸门挡住，而给 Add 加返回值会让那处
// 也必须处理一个永不发生的错误。
func (a *Aggregator) Overflow() string { return a.overflow }

// admit 在累积前记账并判闸门。返回 false 表示这一份增量不再收。
//
// 撞线之后立即停止累积而不是「收完再判」：继续累下去等于上限没有意义。
func (a *Aggregator) admit(n int) bool {
	if a.overflow != "" {
		return false
	}
	if a.contentBytes+n > maxContentBytes {
		a.overflow = "upstream response content exceeds the " +
			strconv.Itoa(maxContentBytes) + " byte limit"
		return false
	}
	a.contentBytes += n
	return true
}

// accOf 取出块的累积器，没有就建一个。
func (a *Aggregator) accOf(index int) *acc {
	if a.accs == nil {
		a.accs = map[int]*acc{}
	}
	if x, ok := a.accs[index]; ok {
		return x
	}
	x := &acc{}
	a.accs[index] = x
	return x
}

func (a *Aggregator) Add(ev Event) {
	if a.blocks == nil {
		a.blocks = map[int]*Block{}
	}
	switch ev.Type {
	case EvMessageStart:
		a.resp.ID = ev.MessageID
		a.resp.Model = ev.Model
		a.mergeServiceTier(ev.ServiceTier)
		if ev.Usage != nil {
			a.mergeUsage(*ev.Usage)
		}
	case EvBlockStart:
		b := Block{Type: BlockText}
		if ev.Block != nil {
			b = *ev.Block
		}
		if _, seen := a.blocks[ev.Index]; !seen {
			if len(a.order) >= maxBlocks {
				a.overflow = "upstream response exceeds the " +
					strconv.Itoa(maxBlocks) + " content block limit"
				return
			}
			a.order = append(a.order, ev.Index)
		}
		a.blocks[ev.Index] = &b
		// 上游复用同一索引开新块时累积器必须重置，否则新块会继承上一个块的
		// 内容——blocks 那一行是整体替换，累积器不跟着换就对不上了。
		delete(a.accs, ev.Index)
		// 块开始事件可能自带内容，它要先进 Builder：物化是赋值式的，
		// Builder 里不是全量就会把这部分抹掉。
		a.seed(ev.Index, &b)
	case EvBlockStop:
		if a.closed == nil {
			a.closed = map[int]bool{}
		}
		a.closed[ev.Index] = true
	case EvTextDelta:
		if !a.admit(len(ev.Text)) {
			return
		}
		if a.block(ev.Index, BlockText) != nil {
			a.accOf(ev.Index).text.WriteString(ev.Text)
		}
	case EvThinkingDelta:
		if !a.admit(len(ev.Text)) {
			return
		}
		if b := a.block(ev.Index, BlockThinking); b != nil {
			if b.Thinking == nil {
				b.Thinking = &Thinking{}
			}
			a.accOf(ev.Index).thinking.WriteString(ev.Text)
		}
	case EvSigDelta:
		if !a.admit(len(ev.Text)) {
			return
		}
		if b := a.block(ev.Index, BlockThinking); b != nil {
			if b.Thinking == nil {
				b.Thinking = &Thinking{}
			}
			a.accOf(ev.Index).sig.WriteString(ev.Text)
			// 空来源不覆盖：块开始事件可能已经带了来源，用空值抹掉它会让
			// 聚合路径判不出同族，把异族签名放行。
			if ev.SignatureFrom != "" {
				b.Thinking.SignatureFrom = ev.SignatureFrom
			}
		}
	case EvToolInput:
		if !a.admit(len(ev.Text)) {
			return
		}
		if b := a.block(ev.Index, BlockToolUse); b != nil {
			if b.ToolUse == nil {
				b.ToolUse = &ToolUse{}
			}
			a.accOf(ev.Index).input.WriteString(ev.Text)
			if ev.Text != "" {
				if a.sawInput == nil {
					a.sawInput = map[int]bool{}
				}
				a.sawInput[ev.Index] = true
			}
		}
	case EvMessageDelta:
		if ev.StopReason != "" {
			a.resp.StopReason = ev.StopReason
		}
		if ev.StopSequence != "" {
			a.resp.StopSequence = ev.StopSequence
		}
		a.mergeServiceTier(ev.ServiceTier)
		if ev.Usage != nil {
			a.mergeUsage(*ev.Usage)
		}
	}
}

// block 取出目标块；上游漏发 block_start 时按 delta 类型补开一个，
// 否则整段内容会被静默丢弃。撞块数上限时返回 nil，调用方一律已判空。
func (a *Aggregator) block(index int, kind BlockType) *Block {
	if b, ok := a.blocks[index]; ok {
		return b
	}
	if len(a.order) >= maxBlocks {
		a.overflow = "upstream response exceeds the " +
			strconv.Itoa(maxBlocks) + " content block limit"
		return nil
	}
	b := &Block{Type: kind}
	a.blocks[index] = b
	a.order = append(a.order, index)
	return b
}

// seed 把块开始事件自带的内容灌进累积器。
//
// 这些字节也要记账：它们和增量一样是上游发来的内容，漏记会让一个「每个块都
// 自带 1 MiB 正文」的上游绕过字节闸门。
func (a *Aggregator) seed(index int, b *Block) {
	n := len(b.Text)
	if b.Thinking != nil {
		n += len(b.Thinking.Text) + len(b.Thinking.Signature)
	}
	if b.ToolUse != nil {
		n += len(b.ToolUse.Input)
	}
	if !a.admit(n) {
		return
	}
	x := a.accOf(index)
	x.text.WriteString(b.Text)
	if b.Thinking != nil {
		x.thinking.WriteString(b.Thinking.Text)
		x.sig.WriteString(b.Thinking.Signature)
	}
	if b.ToolUse != nil {
		x.input.WriteString(b.ToolUse.Input)
	}
}

// materialize 把累积器里的内容写回块上。
//
// 用赋值而不是追加，因此天然幂等：Response 会被调用多次（bridge 判完
// StopReason 之后 writeSuccess 里还会再调一次），而三个出口
// （Response / IncompleteTools / UnsafeToClose）各自都要先调它——后两者读的是
// ToolUse.Input 与 Thinking.Signature，不物化就会看到空串，于是每个带入参的
// 工具调用都被判成截断。
//
// Builder 因此必须始终持有全量，中途不能交出所有权。String() 不复制底层字节，
// 重复调用的代价可忽略。
func (a *Aggregator) materialize() {
	for idx, x := range a.accs {
		b := a.blocks[idx]
		if b == nil {
			continue
		}
		b.Text = x.text.String()
		if b.Thinking != nil {
			b.Thinking.Text = x.thinking.String()
			b.Thinking.Signature = x.sig.String()
		}
		if b.ToolUse != nil {
			b.ToolUse.Input = x.input.String()
		}
	}
}

func (a *Aggregator) mergeUsage(u Usage) { MergeUsage(&a.resp.Usage, u) }

// mergeServiceTier 用「非空覆盖」，与 usage 的输入输出维度同口径：
// 后到的那份更完整，而缺了这一维的帧不该把已收到的值清零。
func (a *Aggregator) mergeServiceTier(tier string) {
	if tier != "" {
		a.resp.ServiceTier = tier
	}
}

// IncompleteTools 返回入参被截断的工具调用 id。
//
// 判定条件是「收到过入参增量，但累积值不是合法 JSON」：上游在参数发到一半
// 断流就是这个形态。这种响应不能当成功——客户端会把残缺调用存进历史，
// 下一轮重放时整个请求都会被上游拒收。
func (a *Aggregator) IncompleteTools() []string {
	a.materialize()
	var out []string
	for _, idx := range a.order {
		if !a.sawInput[idx] {
			continue
		}
		b := a.blocks[idx]
		if b == nil || b.Type != BlockToolUse || b.ToolUse == nil {
			continue
		}
		if json.Valid([]byte(b.ToolUse.Input)) {
			continue
		}
		out = append(out, toolLabel(b.ToolUse, idx))
	}
	return out
}

// UnsafeToClose 返回不能照常收尾的块标签。
//
// 两类：入参被截断的工具调用，以及断流时仍开着且没拿到签名的 thinking 块。
// 缺签名的推理块回传给上游会被判为伪造而整轮拒收，和残缺入参一样属于
// 「补个闭合帧就变成一条有毒历史」，必须让客户端知道这次没说完。
// 纯文本块不在其中——半句话是可接受的截断，由 max_tokens 表达。
func (a *Aggregator) UnsafeToClose() []string {
	// IncompleteTools 里已经物化过一次，但这里不能省掉显式调用：thinking 签名
	// 那一路的判定在下面，而「上面那个函数恰好也物化」是它的实现细节。
	a.materialize()
	out := a.IncompleteTools()
	for _, idx := range a.order {
		if a.closed[idx] {
			continue
		}
		b := a.blocks[idx]
		if b == nil || b.Type != BlockThinking {
			continue
		}
		if b.Thinking != nil && b.Thinking.Signature != "" {
			continue
		}
		out = append(out, "thinking block "+strconv.Itoa(idx))
	}
	return out
}

// HasOpenBlocks 报告是否有块在流结束时仍未收到闭合帧。
func (a *Aggregator) HasOpenBlocks() bool {
	for _, idx := range a.order {
		if !a.closed[idx] {
			return true
		}
	}
	return false
}

// toolLabel 给截断的调用取一个可诊断的标签。
func toolLabel(use *ToolUse, index int) string {
	if use.ID != "" {
		return use.ID
	}
	if use.Name != "" {
		return use.Name
	}
	return "block " + strconv.Itoa(index)
}

func (a *Aggregator) Response() *Response {
	a.materialize()
	out := a.resp
	out.Content = make([]Block, 0, len(a.order))
	hasToolUse := false
	for _, idx := range a.order {
		if b := a.blocks[idx]; b != nil {
			out.Content = append(out.Content, *b)
			if b.Type == BlockToolUse {
				hasToolUse = true
			}
		}
	}
	// 内容里确有工具调用，终止原因就必须表达为工具调用：客户端靠它决定
	// 要不要执行工具，判成 end_turn 会让整个工具回合悄悄断在这里。
	// content_filter 不改判——被拦截的工具调用不该被执行。
	if hasToolUse && out.StopReason != StopContentFilter {
		out.StopReason = StopToolUse
	}
	return &out
}
