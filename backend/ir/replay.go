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
	out := []Event{{Type: EvMessageStart, MessageID: resp.ID, Model: resp.Model,
		ServiceTier: resp.ServiceTier, Container: resp.Container, Audio: resp.Audio}}
	for i, b := range resp.Content {
		out = append(out, blockEvents(i, b)...)
	}
	usage := resp.Usage
	out = append(out, Event{Type: EvMessageDelta, StopReason: resp.StopReason,
		StopSequence: resp.StopSequence, Usage: &usage})
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
	// 引用改走 EvCitation：流式协议一律在正文之后单独下发标注
	// （Anthropic 的 citations_delta、Chat 的 delta.annotations），
	// 留在骨架上会让编码器在正文还没发出时就写出偏移量，反推全部失败。
	cites := b.Citations
	b.Citations = nil
	switch b.Type {
	case BlockText, BlockRefusal:
		// 拒绝块也用 Text 承载。漏掉这一档会让「上游非流式、客户端流式」
		// 这条路径只发出空的块开合，拒绝正文整条不见。
		if b.Text != "" {
			deltas = append(deltas, Event{Type: EvTextDelta, Index: index, Text: b.Text})
		}
		b.Text = ""
		if len(cites) > 0 {
			deltas = append(deltas, Event{Type: EvCitation, Index: index, Citations: cites})
		}
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
	case BlockServerToolUse:
		// 托管工具调用的查询串与普通入参同一条增量通道：流式编码器约定
		// 开启帧的 input 为空对象，整串留在块上会被下游丢掉。
		if b.ServerToolUse != nil {
			s := *b.ServerToolUse
			if s.Input != "" {
				deltas = append(deltas, Event{Type: EvToolInput, Index: index, Text: s.Input})
			}
			s.Input = ""
			b.ServerToolUse = &s
		}
	}
	return b, deltas
}
