package ir

import (
	"strconv"
	"strings"
	"testing"
)

// 本文件守三道累积闸门里属于聚合器的两道（字节与块数）。判据 6–11。
//
// 此前整条流的累计字节没有任何上限：单帧上限（32 MiB）乘以无限帧数等于没有
// 上限，实测 64 帧 × 1 MiB 累到 64 MiB 时一道闸门都没触发。三个参考仓库
// （new-api / sub2api / cc-switch）全都没有这类整流闸门，判据是自定的。

// 判据 6：上限以内不触发。
//
// 与判据 7 成对：只测触发那一格的话，把闸门改成「恒触发」也是绿的。
func TestBytesUnderLimitDoesNotOverflow(t *testing.T) {
	var a Aggregator
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{Type: BlockText}})
	a.Add(Event{Type: EvTextDelta, Index: 0, Text: strings.Repeat("x", maxContentBytes)})

	if got := a.Overflow(); got != "" {
		t.Errorf("恰好到上限就报了 overflow = %q，want 空——上限的含义是"+
			"「最多这么多」，等于不算超", got)
	}
	if n := len(a.Response().Content[0].Text); n != maxContentBytes {
		t.Errorf("内容字节 = %d，want %d", n, maxContentBytes)
	}
}

// 判据 7：超出一个字节即触发。
func TestBytesOverLimitOverflows(t *testing.T) {
	var a Aggregator
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{Type: BlockText}})
	a.Add(Event{Type: EvTextDelta, Index: 0, Text: strings.Repeat("x", maxContentBytes)})
	a.Add(Event{Type: EvTextDelta, Index: 0, Text: "x"})

	if a.Overflow() == "" {
		t.Fatal("超过上限一个字节没有触发闸门")
	}
}

// 判据 7 续：跨帧累计，而不是每帧各判一次。
//
// 这条是本轮的核心缺口——每帧 1 MiB 远在单帧上限之下，此前一路放行。
func TestBytesAccumulateAcrossFrames(t *testing.T) {
	var a Aggregator
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{Type: BlockText}})
	chunk := strings.Repeat("x", 1<<20)
	for i := 0; i < 33 && a.Overflow() == ""; i++ {
		a.Add(Event{Type: EvTextDelta, Index: 0, Text: chunk})
	}

	if a.Overflow() == "" {
		t.Error("33 帧 × 1 MiB 累到 33 MiB 仍未触发闸门——每帧都在单帧上限" +
			"之下，闸门必须看累计值")
	}
}

// 判据 8：块开始事件自带的内容也记账。
//
// 漏记的话，一个「每个块都自带大段正文、之后不发 delta」的上游能整个绕过闸门。
func TestBlockStartContentCountsTowardBytes(t *testing.T) {
	var a Aggregator
	body := strings.Repeat("x", 1<<20)
	for i := 0; i < 33 && a.Overflow() == ""; i++ {
		a.Add(Event{Type: EvBlockStart, Index: i, Block: &Block{Type: BlockText, Text: body}})
	}

	if a.Overflow() == "" {
		t.Error("33 个各带 1 MiB 正文的块未触发字节闸门——seed 的字节没记账")
	}
}

// 判据 9：置位之后不再累积。
//
// 「收完再判」等于上限没有意义：内存已经吃下去了。
func TestOverflowStopsFurtherAccumulation(t *testing.T) {
	var a Aggregator
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{Type: BlockText}})
	a.Add(Event{Type: EvTextDelta, Index: 0, Text: strings.Repeat("x", maxContentBytes)})
	a.Add(Event{Type: EvTextDelta, Index: 0, Text: "over"})
	before := len(a.Response().Content[0].Text)
	a.Add(Event{Type: EvTextDelta, Index: 0, Text: strings.Repeat("y", 1<<20)})

	if after := len(a.Response().Content[0].Text); after != before {
		t.Errorf("置位后又累了 %d 字节（%d → %d）", after-before, before, after)
	}
}

// 判据 9 续：块数闸门置位后同样停止累积内容。
//
// 与上一条分开测：撞字节闸门时累计值本身就已越线，「置位即停」那一步删掉也
// 看不出差别；而撞块数闸门时累计字节可能还很小，少了那一步就会一路收下去，
// 于是一个疯狂开块的上游仍能把内存吃满。
func TestBlockGateAlsoStopsByteAccumulation(t *testing.T) {
	var a Aggregator
	for i := 0; i <= maxBlocks; i++ {
		a.Add(Event{Type: EvBlockStart, Index: i, Block: &Block{Type: BlockText}})
	}
	if a.Overflow() == "" {
		t.Fatal("夹具没能撞上块数闸门")
	}
	before := a.contentBytes
	a.Add(Event{Type: EvTextDelta, Index: 0, Text: strings.Repeat("y", 1<<20)})

	if after := a.contentBytes; after != before {
		t.Errorf("块数闸门置位后又累了 %d 字节（%d → %d）", after-before, before, after)
	}
}

// 判据 9 续：四种增量都受闸门约束。
//
// 四个 case 各自调 admit，只测文本那一路的话另三处漏掉在跑一次的测试里是绿的。
func TestAllDeltaKindsRespectTheGate(t *testing.T) {
	kinds := []struct {
		name string
		ev   Event
	}{
		{"text", Event{Type: EvTextDelta, Index: 0, Text: "x"}},
		{"thinking", Event{Type: EvThinkingDelta, Index: 1, Text: "x"}},
		{"signature", Event{Type: EvSigDelta, Index: 1, Text: "x"}},
		{"tool input", Event{Type: EvToolInput, Index: 2, Text: "x"}},
	}
	for _, k := range kinds {
		t.Run(k.name, func(t *testing.T) {
			var a Aggregator
			a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{Type: BlockText}})
			a.Add(Event{Type: EvBlockStart, Index: 1, Block: &Block{Type: BlockThinking}})
			a.Add(Event{Type: EvBlockStart, Index: 2, Block: &Block{Type: BlockToolUse,
				ToolUse: &ToolUse{ID: "c1"}}})
			a.Add(Event{Type: EvTextDelta, Index: 0,
				Text: strings.Repeat("x", maxContentBytes)})
			if a.Overflow() != "" {
				t.Fatal("填满阶段就触发了，夹具构造有误")
			}
			a.Add(k.ev)
			if a.Overflow() == "" {
				t.Errorf("%s 增量越过了字节闸门", k.name)
			}
		})
	}
}

// 判据 10：块数闸门。
//
// 块索引是上游给的，本服务不校验它；一个索引跳号的上游 bug 会变成本服务的
// 内存增长。
func TestBlockCountGate(t *testing.T) {
	var a Aggregator
	for i := 0; i < maxBlocks; i++ {
		a.Add(Event{Type: EvBlockStart, Index: i, Block: &Block{Type: BlockText}})
	}
	if a.Overflow() != "" {
		t.Fatalf("恰好 %d 个块就触发了：%q", maxBlocks, a.Overflow())
	}
	a.Add(Event{Type: EvBlockStart, Index: maxBlocks, Block: &Block{Type: BlockText}})
	if a.Overflow() == "" {
		t.Errorf("第 %d 个块未触发块数闸门", maxBlocks+1)
	}
	if n := len(a.Response().Content); n != maxBlocks {
		t.Errorf("块数 = %d，want %d——撞线那个块不该被收下", n, maxBlocks)
	}
}

// 判据 10 续：补开块那条路径同样受块数闸门约束。
//
// block() 会在上游漏发 block_start 时补开块，那是第二条增长块数的路径。
func TestImplicitBlocksRespectTheCountGate(t *testing.T) {
	var a Aggregator
	for i := 0; i < maxBlocks+10; i++ {
		a.Add(Event{Type: EvTextDelta, Index: i, Text: "x"})
	}
	if a.Overflow() == "" {
		t.Error("补开的块越过了块数闸门")
	}
	if n := len(a.Response().Content); n > maxBlocks {
		t.Errorf("块数 = %d，超过上限 %d", n, maxBlocks)
	}
}

// 判据 11：说明里带闸门名与数值，不带内容片段。
//
// 带数值是为了让运维看到撞的是哪道闸门、阈值是多少；不带内容是因为这条说明
// 会进流水 error_message、Redis 实时环、管理面与客户端可见的错误体四个出口，
// 而内容可能含客户端的私有数据（与出站请求头黑名单同口径）。
func TestOverflowMessageNamesTheGateWithoutContent(t *testing.T) {
	var a Aggregator
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{Type: BlockText}})
	a.Add(Event{Type: EvTextDelta, Index: 0,
		Text: strings.Repeat("SECRETPAYLOAD", maxContentBytes/13+1)})

	msg := a.Overflow()
	if !strings.Contains(msg, strconv.Itoa(maxContentBytes)) {
		t.Errorf("字节闸门说明 %q 里没有阈值 %d", msg, maxContentBytes)
	}
	if !strings.Contains(msg, "byte limit") {
		t.Errorf("字节闸门说明 %q 没说是哪道闸门", msg)
	}
	if strings.Contains(msg, "SECRETPAYLOAD") {
		t.Errorf("字节闸门说明里带上了内容片段：%q", msg)
	}

	var b Aggregator
	for i := 0; i <= maxBlocks; i++ {
		b.Add(Event{Type: EvBlockStart, Index: i,
			Block: &Block{Type: BlockText, Text: "SECRETPAYLOAD"}})
	}
	bmsg := b.Overflow()
	if !strings.Contains(bmsg, strconv.Itoa(maxBlocks)) {
		t.Errorf("块数闸门说明 %q 里没有阈值 %d", bmsg, maxBlocks)
	}
	if !strings.Contains(bmsg, "content block limit") {
		t.Errorf("块数闸门说明 %q 没说是哪道闸门", bmsg)
	}
	if strings.Contains(bmsg, "SECRETPAYLOAD") {
		t.Errorf("块数闸门说明里带上了内容片段：%q", bmsg)
	}
	if bmsg == msg {
		t.Error("两道闸门的说明逐字相同，运维分不出撞的是哪一道")
	}
}

// 判据 6 续：正常规模的响应一律不触发。
//
// 闸门对绝大多数请求必须是恒等的，否则会悄悄改写全部历史流水。
func TestOrdinaryResponseNeverOverflows(t *testing.T) {
	var a Aggregator
	a.Add(Event{Type: EvMessageStart, MessageID: "msg_1", Model: "k3"})
	for i := 0; i < 3; i++ {
		a.Add(Event{Type: EvBlockStart, Index: i, Block: &Block{Type: BlockText}})
		a.Add(Event{Type: EvTextDelta, Index: i, Text: strings.Repeat("word ", 200)})
		a.Add(Event{Type: EvBlockStop, Index: i})
	}
	if got := a.Overflow(); got != "" {
		t.Errorf("普通规模响应触发了闸门：%q", got)
	}
}
