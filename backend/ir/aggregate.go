package ir

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
}

func NewAggregator() *Aggregator {
	return &Aggregator{blocks: map[int]*Block{}}
}

func (a *Aggregator) Add(ev Event) {
	switch ev.Type {
	case EvMessageStart:
		a.resp.ID = ev.MessageID
		a.resp.Model = ev.Model
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
		}
	case EvToolInput:
		if b := a.block(ev.Index, BlockToolUse); b != nil {
			if b.ToolUse == nil {
				b.ToolUse = &ToolUse{}
			}
			b.ToolUse.Input += ev.Text
		}
	case EvMessageDelta:
		if ev.StopReason != "" {
			a.resp.StopReason = ev.StopReason
		}
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

// mergeUsage 逐字段取较大值：usage 可能分散在 message_start 与 message_delta
// 两帧（前者给 input、后者给 output），且部分上游会重复发送累计值。
func (a *Aggregator) mergeUsage(u Usage) {
	if u.InputTokens > a.resp.Usage.InputTokens {
		a.resp.Usage.InputTokens = u.InputTokens
	}
	if u.OutputTokens > a.resp.Usage.OutputTokens {
		a.resp.Usage.OutputTokens = u.OutputTokens
	}
	if u.CacheReadTokens > a.resp.Usage.CacheReadTokens {
		a.resp.Usage.CacheReadTokens = u.CacheReadTokens
	}
	if u.CacheWriteTokens > a.resp.Usage.CacheWriteTokens {
		a.resp.Usage.CacheWriteTokens = u.CacheWriteTokens
	}
}

func (a *Aggregator) Response() *Response {
	out := a.resp
	out.Content = make([]Block, 0, len(a.order))
	for _, idx := range a.order {
		if b := a.blocks[idx]; b != nil {
			out.Content = append(out.Content, *b)
		}
	}
	return &out
}
