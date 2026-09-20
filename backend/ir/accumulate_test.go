package ir

import (
	"strings"
	"testing"
	"time"
)

// 本文件守聚合器的累积形态。判据 1–5。
//
// 此前四处累积都是往 Block 的字符串字段上 += ，每帧一次全量重分配。实测
// 160000 帧（10 MiB 文本）耗时 3 分 03 秒、累计分配 763 GiB；而这条路径每个
// 请求都走——流式也要靠聚合器算 usage 与判残缺，不只非流式。改成 Builder
// 之后同一探针 14.9 毫秒、49 MiB。

// 判据 1：摊还线性。
//
// 断言的是「4 倍帧数不到 6 倍耗时」而不是一个绝对秒数：绝对值取决于机器，
// 而二次复杂度的特征是倍率——修复前这个比值是 18.8，修复后约 3。
// 阈值取 6 留了两倍余量，同时离 18.8 足够远。
func TestAccumulationIsAmortizedLinear(t *testing.T) {
	if testing.Short() {
		t.Skip("规模测试在 -short 下跳过")
	}
	small := timeAccumulation(10000)
	large := timeAccumulation(40000)
	// 小规模耗时可能落到计时精度以下，抬到 1 微秒避免除零放大噪声。
	if small < time.Microsecond {
		small = time.Microsecond
	}
	if ratio := float64(large) / float64(small); ratio > 6 {
		t.Errorf("帧数翻四倍耗时涨了 %.1f 倍（%v → %v）——超过线性太多，"+
			"说明累积又变回每帧一次全量拷贝", ratio, small, large)
	}
}

func timeAccumulation(frames int) time.Duration {
	var a Aggregator
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{Type: BlockText}})
	chunk := strings.Repeat("x", 64)
	start := time.Now()
	for i := 0; i < frames; i++ {
		a.Add(Event{Type: EvTextDelta, Index: 0, Text: chunk})
	}
	_ = a.Response()
	return time.Since(start)
}

// 判据 2：物化幂等——Response 连调两次内容不翻倍。
//
// 物化是赋值式的，追加式实现会在第二次调用时把内容拼两遍。而 bridge 确实会
// 调多次（判 StopReason 之后 writeSuccess 里还有一次）。
func TestResponseIsIdempotent(t *testing.T) {
	var a Aggregator
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{Type: BlockText}})
	a.Add(Event{Type: EvTextDelta, Index: 0, Text: "hello"})

	first := a.Response().Content[0].Text
	second := a.Response().Content[0].Text

	if first != "hello" || second != "hello" {
		t.Errorf("两次 Response 的文本 = %q / %q，want 都是 %q", first, second, "hello")
	}
}

// 判据 3：三个出口各自物化。
//
// IncompleteTools 与 UnsafeToClose 读的是 ToolUse.Input 与 Thinking.Signature，
// 不物化就看到空串：前者会把每个带入参的工具调用判成截断，后者会把拿到签名的
// 推理块判成缺签名。两者都是「把可用响应判成不可用」。
func TestIncompleteToolsSeesMaterializedInput(t *testing.T) {
	var a Aggregator
	a.Add(Event{Type: EvBlockStart, Index: 0,
		Block: &Block{Type: BlockToolUse, ToolUse: &ToolUse{ID: "call_1", Name: "ls"}}})
	a.Add(Event{Type: EvToolInput, Index: 0, Text: `{"path":`})
	a.Add(Event{Type: EvToolInput, Index: 0, Text: `"/tmp"}`})

	if got := a.IncompleteTools(); len(got) != 0 {
		t.Errorf("IncompleteTools = %v，want 空——入参拼起来是合法 JSON，"+
			"报出来说明这个出口没物化就去读 Input 了", got)
	}
}

func TestUnsafeToCloseSeesMaterializedSignature(t *testing.T) {
	var a Aggregator
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{Type: BlockThinking}})
	a.Add(Event{Type: EvSigDelta, Index: 0, Text: "sig-", SignatureFrom: "anthropic"})
	a.Add(Event{Type: EvSigDelta, Index: 0, Text: "part2"})

	if got := a.UnsafeToClose(); len(got) != 0 {
		t.Errorf("UnsafeToClose = %v，want 空——签名拼起来非空，报出来说明"+
			"这个出口读到的是未物化的空串", got)
	}
	if sig := a.Response().Content[0].Thinking.Signature; sig != "sig-part2" {
		t.Errorf("签名 = %q，want %q", sig, "sig-part2")
	}
}

// 判据 4：块开始事件自带的内容不被物化抹掉。
//
// 物化是赋值式的，所以 Builder 必须始终持有全量。seed 漏掉这一步的话，
// 一个「block_start 就带完整文本、之后没有任何 delta」的上游（gemini 非流式
// 经聚合器就是这个形态）内容会被清空。
func TestBlockStartContentSurvivesMaterialize(t *testing.T) {
	var a Aggregator
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{Type: BlockText, Text: "seeded"}})
	a.Add(Event{Type: EvBlockStart, Index: 1, Block: &Block{Type: BlockToolUse,
		ToolUse: &ToolUse{ID: "c1", Input: `{"a":1}`}}})

	resp := a.Response()
	if resp.Content[0].Text != "seeded" {
		t.Errorf("块自带文本 = %q，want %q——物化把 seed 的内容抹掉了",
			resp.Content[0].Text, "seeded")
	}
	if resp.Content[1].ToolUse.Input != `{"a":1}` {
		t.Errorf("块自带入参 = %q，want %q",
			resp.Content[1].ToolUse.Input, `{"a":1}`)
	}
}

// 判据 4 续：自带内容与随后的增量拼接，顺序是自带在前。
func TestBlockStartContentPrecedesDeltas(t *testing.T) {
	var a Aggregator
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{Type: BlockText, Text: "head"}})
	a.Add(Event{Type: EvTextDelta, Index: 0, Text: "-tail"})

	if got := a.Response().Content[0].Text; got != "head-tail" {
		t.Errorf("文本 = %q，want %q", got, "head-tail")
	}
}

// 判据 5：上游复用同一索引开新块时累积器重置。
//
// blocks 那一行是整体替换（`a.blocks[ev.Index] = &b`），累积器不跟着清就会让
// 新块继承上一个块的内容。responses 的 output_index 会跳号，也就可能重复。
func TestReopeningAnIndexResetsAccumulation(t *testing.T) {
	var a Aggregator
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{Type: BlockText}})
	a.Add(Event{Type: EvTextDelta, Index: 0, Text: "first"})
	a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{Type: BlockText}})
	a.Add(Event{Type: EvTextDelta, Index: 0, Text: "second"})

	if got := a.Response().Content[0].Text; got != "second" {
		t.Errorf("重开同一索引后文本 = %q，want %q——累积器没跟着换，"+
			"新块继承了上一个块的内容", got, "second")
	}
}

// 判据 5 续：重开索引不增加块数。
//
// order 只在首次出现时追加，这条守的是那个判断没被改成无条件追加——
// 否则一个反复重开同一索引的上游会把块数闸门撞满。
func TestReopeningAnIndexDoesNotGrowBlockCount(t *testing.T) {
	var a Aggregator
	for i := 0; i < 5; i++ {
		a.Add(Event{Type: EvBlockStart, Index: 0, Block: &Block{Type: BlockText}})
	}
	if n := len(a.Response().Content); n != 1 {
		t.Errorf("重开五次后块数 = %d，want 1", n)
	}
}

// 判据 2 续：漏发 block_start 时补开的块也走累积器。
//
// block() 补开的块没经过 seed，这条守它的累积路径同样接上了 Builder
// （而不是仍然往 Block 字段上 += ，那样物化会把它清空）。
func TestDeltaWithoutBlockStartStillAccumulates(t *testing.T) {
	var a Aggregator
	a.Add(Event{Type: EvTextDelta, Index: 7, Text: "ab"})
	a.Add(Event{Type: EvTextDelta, Index: 7, Text: "cd"})

	resp := a.Response()
	if len(resp.Content) != 1 || resp.Content[0].Text != "abcd" {
		t.Errorf("补开的块内容 = %+v，want 一个文本块 %q", resp.Content, "abcd")
	}
}
