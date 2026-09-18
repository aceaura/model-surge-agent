package ir

import (
	"encoding/json"
	"strconv"
)

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
			a.order = append(a.order, ev.Index)
		}
		a.blocks[ev.Index] = &b
	case EvBlockStop:
		if a.closed == nil {
			a.closed = map[int]bool{}
		}
		a.closed[ev.Index] = true
	case EvTextDelta:
		if b := a.block(ev.Index, BlockText); b != nil {
			b.Text += ev.Text
		}
	case EvThinkingDelta:
		if b := a.block(ev.Index, BlockThinking); b != nil {
			if b.Thinking == nil {
				b.Thinking = &Thinking{}
			}
			b.Thinking.Text += ev.Text
		}
	case EvSigDelta:
		if b := a.block(ev.Index, BlockThinking); b != nil {
			if b.Thinking == nil {
				b.Thinking = &Thinking{}
			}
			b.Thinking.Signature += ev.Text
			// 空来源不覆盖：块开始事件可能已经带了来源，用空值抹掉它会让
			// 聚合路径判不出同族，把异族签名放行。
			if ev.SignatureFrom != "" {
				b.Thinking.SignatureFrom = ev.SignatureFrom
			}
		}
	case EvToolInput:
		if b := a.block(ev.Index, BlockToolUse); b != nil {
			if b.ToolUse == nil {
				b.ToolUse = &ToolUse{}
			}
			b.ToolUse.Input += ev.Text
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
		a.mergeServiceTier(ev.ServiceTier)
		if ev.Usage != nil {
			a.mergeUsage(*ev.Usage)
		}
	}
}

// block 取出目标块；上游漏发 block_start 时按 delta 类型补开一个，
// 否则整段内容会被静默丢弃。
func (a *Aggregator) block(index int, kind BlockType) *Block {
	if b, ok := a.blocks[index]; ok {
		return b
	}
	b := &Block{Type: kind}
	a.blocks[index] = b
	a.order = append(a.order, index)
	return b
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
