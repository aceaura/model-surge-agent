package ir

// ResponseEvents 把一份完整响应投影成事件序列，与 Aggregator 反向。
//
// 数据面对上游一律请求流式，但上游可能忽略它、回一整份 JSON。此时把响应
// 投影成事件，下游（四个入站编码器与 Aggregator）就不必各自再多一条
// 非流式分支——走的是与真流式完全相同的路径。
//
// 内容放在增量事件里而不是 block_start 的块上：流式编码器对 block_start
// 的处理是「开块」，四个协议都不从它读正文，塞进去等于依赖一个编码器
// 不保证的行为。
func ResponseEvents(resp *Response) []Event {
	if resp == nil {
		return nil
	}
	out := []Event{{Type: EvMessageStart, MessageID: resp.ID, Model: resp.Model}}
	for i, b := range resp.Content {
		out = append(out, blockEvents(i, b)...)
	}
	usage := resp.Usage
	out = append(out, Event{Type: EvMessageDelta, StopReason: resp.StopReason, Usage: &usage})
	return append(out, Event{Type: EvMessageStop})
}

func blockEvents(index int, b Block) []Event {
	skeleton, deltas := splitBlock(index, b)
	out := make([]Event, 0, len(deltas)+2)
	out = append(out, Event{Type: EvBlockStart, Index: index, Block: &skeleton})
	out = append(out, deltas...)
	return append(out, Event{Type: EvBlockStop, Index: index})
}

// splitBlock 把块拆成「不含正文的骨架」与「承载正文的增量」。
func splitBlock(index int, b Block) (Block, []Event) {
	var deltas []Event
	switch b.Type {
	case BlockText:
		if b.Text != "" {
			deltas = append(deltas, Event{Type: EvTextDelta, Index: index, Text: b.Text})
		}
		b.Text = ""
	case BlockThinking:
		if b.Thinking != nil {
			t := *b.Thinking
			if t.Text != "" {
				deltas = append(deltas, Event{Type: EvThinkingDelta, Index: index, Text: t.Text})
			}
			if t.Signature != "" {
				deltas = append(deltas, Event{Type: EvSigDelta, Index: index,
					Text: t.Signature, SignatureFrom: t.SignatureFrom})
			}
			t.Text = ""
			t.Signature = ""
			b.Thinking = &t
		}
	case BlockToolUse:
		if b.ToolUse != nil {
			u := *b.ToolUse
			if u.Input != "" {
				// 入参走增量而不留在块上：Aggregator 的截断判定只认增量，
				// 留在块里会让残缺入参判不出来——非流式上游给的入参同样
				// 可能是残缺 JSON。
				deltas = append(deltas, Event{Type: EvToolInput, Index: index, Text: u.Input})
			}
			u.Input = ""
			b.ToolUse = &u
		}
	}
	return b, deltas
}
