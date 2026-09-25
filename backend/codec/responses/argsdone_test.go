package responses

import (
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// function_call_arguments.done 携带的是完整参数终态而不是新一份参数。
// 只发终态不发增量的上游，全部入参只在这一帧（或 output_item.done 的
// item.arguments）里出现，整帧丢掉客户端就拿到一个零入参的调用。
// 判据与正文回补同源：终态是已发内容的延长才补缺失后缀。

func argsOf(t *testing.T, resp *ir.Response) string {
	t.Helper()
	for _, blk := range resp.Content {
		if blk.Type == ir.BlockToolUse && blk.ToolUse != nil {
			return blk.ToolUse.Input
		}
	}
	t.Fatalf("响应里没有工具调用块：%+v", resp.Content)
	return ""
}

// done-only：开启帧不带参数、增量一帧没有，done 给出全部入参；
// 随后 item.done 带同一份参数不得重复下发。
func TestStreamDecodeFunctionArgumentsDoneWithoutDeltas(t *testing.T) {
	d := newStreamDecoder()
	if _, err := d.Feed("", `{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"lookup"}}`); err != nil {
		t.Fatal(err)
	}
	evs, err := d.Feed("", `{"type":"response.function_call_arguments.done","output_index":1,"arguments":"{\"id\":1}"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != ir.EvToolInput || evs[0].Text != `{"id":1}` {
		t.Fatalf("done-only 事件 = %+v", evs)
	}
	evs, err = d.Feed("", `{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"id\":1}"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != ir.EvBlockStop {
		t.Fatalf("item.done 把同一份参数又发了一遍：%+v", evs)
	}
}

// 增量只来了一半：done 只补缺失后缀。
func TestStreamDecodeFunctionArgumentsDoneCompletesDeltaPrefix(t *testing.T) {
	d := newStreamDecoder()
	if _, err := d.Feed("", `{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"id\":"}`); err != nil {
		t.Fatal(err)
	}
	evs, err := d.Feed("", `{"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"id\":1}"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != ir.EvToolInput || evs[0].Text != `1}` {
		t.Fatalf("done 后缀事件 = %+v（重复前缀或丢后缀都算失败）", evs)
	}
	// 重复到来的同一份 done 不得再产出增量。
	evs, err = d.Feed("", `{"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"id\":1}"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 0 {
		t.Fatalf("重复的 done 又下发了参数：%+v", evs)
	}
}

// 终态比已发内容短、或与已发内容分叉：都不得追加成畸形 JSON。
func TestStreamDecodeFunctionArgumentsDoneRejectsNonSuffix(t *testing.T) {
	for _, full := range []string{`{"id":`, `{"name":1}`} {
		d := newStreamDecoder()
		if _, err := d.Feed("", `{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"id\":1}"}`); err != nil {
			t.Fatal(err)
		}
		evs, err := d.Feed("", `{"type":"response.function_call_arguments.done","output_index":0,"arguments":`+quoteJSON(full)+`}`)
		if err != nil {
			t.Fatal(err)
		}
		if len(evs) != 0 {
			t.Fatalf("非后缀终态 %q 被追加：%+v", full, evs)
		}
	}
}

// 完整参数只在 output_item.done 的 item.arguments 里：回补后聚合出完整入参，
// 且参数增量排在闭块之前。
func TestStreamDecodeOutputItemDoneBackfillsArguments(t *testing.T) {
	d := newStreamDecoder()
	if _, err := d.Feed("", `{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","call_id":"call_2","name":"lookup"}}`); err != nil {
		t.Fatal(err)
	}
	evs, err := d.Feed("", `{"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","call_id":"call_2","name":"lookup","arguments":"{\"id\":2}"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Type != ir.EvToolInput || evs[0].Text != `{"id":2}` || evs[1].Type != ir.EvBlockStop {
		t.Fatalf("item.done 事件 = %+v", evs)
	}
}

// 开启帧就带完整参数（非增量实现）：随后的 done 带同一份不得重复。
func TestStreamDecodeOutputItemAddedCarriesArguments(t *testing.T) {
	d := newStreamDecoder()
	evs, err := d.Feed("", `{"type":"response.output_item.added","output_index":3,"item":{"type":"function_call","call_id":"call_3","name":"lookup","arguments":"{\"id\":3}"}}`)
	if err != nil {
		t.Fatal(err)
	}
	// 首帧会同时补出 message_start，断言尾部两个事件。
	if len(evs) != 3 || evs[1].Type != ir.EvBlockStart || evs[2].Type != ir.EvToolInput || evs[2].Text != `{"id":3}` {
		t.Fatalf("开启帧事件 = %+v", evs)
	}
	evs, err = d.Feed("", `{"type":"response.function_call_arguments.done","output_index":3,"arguments":"{\"id\":3}"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 0 {
		t.Fatalf("开启帧已给的参数被 done 又发了一遍：%+v", evs)
	}
}

// 端到端：done-only 调用聚合出完整入参。
func TestStreamDecodeDoneOnlyToolCallAggregatesFullInput(t *testing.T) {
	resp, _ := feedEvents(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"lookup"}}`,
		`{"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"id\":1}"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"id\":1}"}}`,
		dbCompleted,
	)
	if got := argsOf(t, resp); got != `{"id":1}` {
		t.Fatalf("入参 = %q, want %q", got, `{"id":1}`)
	}
}

func quoteJSON(s string) string {
	out := `"`
	for _, r := range s {
		if r == '"' {
			out += `\"`
		} else {
			out += string(r)
		}
	}
	return out + `"`
}
