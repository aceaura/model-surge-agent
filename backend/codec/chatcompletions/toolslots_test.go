package chatcompletions

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 本文件守本协议的工具槽位上限与入参累积形态。判据 16–20。
//
// 槽位由上游给的 index 决定键，本服务不校验它：一个索引跳号的上游 bug 会变成
// 本服务的 map 无界增长。而入参此前是往槽位的字符串字段上 += ，分片数同样
// 由上游决定。

// feedDelta 发一帧带若干 tool_calls 的增量。
func feedDelta(t *testing.T, d *streamDecoder, calls string) ([]ir.Event, error) {
	t.Helper()
	return d.Feed("", `{"choices":[{"delta":{"tool_calls":[`+calls+`]}}]}`)
}

// 判据 16：槽位上限以内不报错。
//
// 与判据 17 成对：只测触发那一格的话，改成恒报错也是绿的。
func TestToolSlotsUnderLimit(t *testing.T) {
	d := newStreamDecoder()
	for i := 0; i < maxToolSlots; i++ {
		if _, err := feedDelta(t, d, toolCallFrag(i, "ls", "{}")); err != nil {
			t.Fatalf("第 %d 个槽位就报错了：%v", i, err)
		}
	}
}

// 判据 17：超过上限时整条请求失败。
//
// 报错而不是静默跳过：跳过会让客户端收到一份少了若干工具调用的 HTTP 200，
// 而那些调用上游确实决定要发。
func TestToolSlotsOverLimitFails(t *testing.T) {
	d := newStreamDecoder()
	for i := 0; i < maxToolSlots; i++ {
		if _, err := feedDelta(t, d, toolCallFrag(i, "ls", "{}")); err != nil {
			t.Fatalf("填充阶段报错：%v", err)
		}
	}
	_, err := feedDelta(t, d, toolCallFrag(maxToolSlots, "ls", "{}"))
	if err == nil {
		t.Fatalf("第 %d 个槽位没有报错", maxToolSlots+1)
	}
	msg := err.Error()
	if !strings.Contains(msg, strconv.Itoa(maxToolSlots)) {
		t.Errorf("错误消息 %q 里没有阈值 %d", msg, maxToolSlots)
	}
	if !strings.Contains(msg, "tool call limit") {
		t.Errorf("错误消息 %q 没说撞的是哪道闸门", msg)
	}
}

// 判据 18：入参分片累积语义不变。
//
// 把 pending 从字符串换成 Builder 是纯形态改动，拼出来的入参必须逐字相同。
func TestPendingArgumentsAccumulate(t *testing.T) {
	d := newStreamDecoder()
	// 先发不带 name 的分片：那条路径才会攒进 pending（有 name 就直接宣告了）。
	if _, err := feedDelta(t, d, `{"index":0,"id":"call_1","function":{"arguments":"{\"a\":"}}`); err != nil {
		t.Fatalf("feed: %v", err)
	}
	if _, err := feedDelta(t, d, `{"index":0,"function":{"arguments":"1}"}}`); err != nil {
		t.Fatalf("feed: %v", err)
	}
	events, err := feedDelta(t, d, `{"index":0,"function":{"name":"ls"}}`)
	if err != nil {
		t.Fatalf("feed: %v", err)
	}

	var got string
	for _, ev := range events {
		if ev.Type == ir.EvToolInput {
			got += ev.Text
		}
	}
	if got != `{"a":1}` {
		t.Errorf("宣告时放出的入参 = %q，want %q——攒在 pending 里的分片"+
			"没有全部放出", got, `{"a":1}`)
	}
}

// 判据 19：pending 放出之后清空，不会重复发一遍。
//
// Builder 的 Reset 漏掉的话，后续分片到达时会把此前攒的入参再发一次，
// 拼出来是 `{"a":1}{"a":1}` 这样的非法 JSON。
func TestPendingIsClearedAfterAnnounce(t *testing.T) {
	d := newStreamDecoder()
	if _, err := feedDelta(t, d, `{"index":0,"id":"call_1","function":{"arguments":"{\"a\":1}"}}`); err != nil {
		t.Fatalf("feed: %v", err)
	}
	if _, err := feedDelta(t, d, `{"index":0,"function":{"name":"ls"}}`); err != nil {
		t.Fatalf("feed: %v", err)
	}
	// 后续分片必须非空：宣告之后那条分支有 `arguments != ""` 的守卫，
	// 空串本就不产出事件，用它做断言看不出 Reset 有没有被删。
	events, err := feedDelta(t, d, `{"index":0,"function":{"arguments":"{\"b\":2}"}}`)
	if err != nil {
		t.Fatalf("feed: %v", err)
	}
	var got string
	for _, ev := range events {
		if ev.Type == ir.EvToolInput {
			got += ev.Text
		}
	}
	if got != `{"b":2}` {
		t.Errorf("宣告后的分片放出 = %q，want %q——pending 没清，"+
			"此前攒的入参被再发了一遍", got, `{"b":2}`)
	}
}

// 判据 20：流结束时残缺入参原样保留。
//
// 清空成 {} 会把一次截断的调用伪装成合法的无参调用——工具真会不带参数
// 执行，那是一次真实副作用。原文留在 IR 里，聚合器的 IncompleteTools
// 据此把这次响应判成不能当成功。
func TestTruncatedPendingKeepsRaw(t *testing.T) {
	d := newStreamDecoder()
	// 只给残缺入参，name 始终不来：走 announcePending 那条路。
	if _, err := feedDelta(t, d, `{"index":0,"id":"call_1","function":{"arguments":"{\"a\":"}}`); err != nil {
		t.Fatalf("feed: %v", err)
	}

	var agg ir.Aggregator
	for _, ev := range d.Finish() {
		agg.Add(ev)
	}
	resp := agg.Response()
	if len(resp.Content) != 1 || resp.Content[0].ToolUse == nil {
		t.Fatalf("want 一个工具调用块，got %+v", resp.Content)
	}
	input := resp.Content[0].ToolUse.Input
	if input != `{"a":` {
		t.Errorf("残缺入参 = %q，want 原文 %q", input, `{"a":`)
	}
	if incomplete := agg.IncompleteTools(); len(incomplete) != 1 {
		t.Errorf("残缺入参没被 IncompleteTools 判出：%v", incomplete)
	}
}

// 判据 20 续：完整入参不被兜底改写。
//
// 只测残缺那一侧的话，把兜底改成「无条件写 {}」也是绿的，而那会丢掉
// 每一次正常的工具入参。
func TestCompletePendingSurvivesFinish(t *testing.T) {
	d := newStreamDecoder()
	if _, err := feedDelta(t, d, `{"index":0,"id":"call_1","function":{"arguments":"{\"a\":1}"}}`); err != nil {
		t.Fatalf("feed: %v", err)
	}

	var agg ir.Aggregator
	for _, ev := range d.Finish() {
		agg.Add(ev)
	}
	resp := agg.Response()
	if len(resp.Content) != 1 || resp.Content[0].ToolUse == nil {
		t.Fatalf("want 一个工具调用块，got %+v", resp.Content)
	}
	if got := resp.Content[0].ToolUse.Input; got != `{"a":1}` {
		t.Errorf("完整入参 = %q，want %q", got, `{"a":1}`)
	}
}

func toolCallFrag(index int, name, args string) string {
	return fmt.Sprintf(`{"index":%d,"id":"call_%d","function":{"name":%q,"arguments":%q}}`,
		index, index, name, args)
}
