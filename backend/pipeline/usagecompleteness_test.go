package pipeline

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
)

// irUsageFields 是 ir.Usage 当前的维度数。
//
// 钉这个数字不是为了它本身，而是为了让「ir.Usage 加了第六位」这件事在这里变红。
// 只逐字段穷举不够：加字段不会让任何既有断言失败，于是新维度会在 usageOf 里被
// 静默丢掉，症状是客户端收到那个数字而流水与调度层记零——一个所有测试都绿的缺口。
const irUsageFields = 5

// 判据 1：usageOf 必须搬全 ir.Usage 的每一位。
//
// 每一位给不同的非零值：给同一个值时「搬错了字段」与「搬对了」结果相同。
func TestUsageOfCarriesEveryDimension(t *testing.T) {
	if n := reflect.TypeOf(ir.Usage{}).NumField(); n != irUsageFields {
		t.Fatalf("ir.Usage 现在有 %d 位而这里以为是 %d 位。"+
			"新增的那一位要在 usageOf、relayclient.Usage、request_log 与 relay 仓的"+
			"契约上各走一遍，否则它会在跨进程时被静默丢掉。", n, irUsageFields)
	}

	agg := &ir.Aggregator{}
	agg.Add(ir.Event{Type: ir.EvMessageStart})
	agg.Add(ir.Event{Type: ir.EvMessageDelta, Usage: &ir.Usage{
		InputTokens:      11,
		OutputTokens:     22,
		CacheReadTokens:  33,
		CacheWriteTokens: 44,
		ReasoningTokens:  55,
	}})

	got := usageOf(agg)
	want := relayclient.Usage{
		InputTokens:      11,
		OutputTokens:     22,
		CacheReadTokens:  33,
		CacheWriteTokens: 44,
		ReasoningTokens:  55,
	}
	if got != want {
		t.Errorf("usageOf = %+v，want %+v；漏掉的那一位客户端本来就收到了，"+
			"只有流水与调度层记零", got, want)
	}
}

// 判据 2：上报线上的键名。
//
// 跨进程契约，两侧永不互相 import，改 tag 在本仓内完全自洽（写与读同一个 tag），
// 线上效果是调度层那两列永远空。承 R18 outcome 字面值、R19 dispatch_ms、
// R20 capture 键名、R21 轨迹键名——这是第五次遇到同型缺口。
func TestUsageWireKeys(t *testing.T) {
	raw, err := json.Marshal(relayclient.Usage{
		InputTokens:      1,
		OutputTokens:     2,
		CacheReadTokens:  3,
		CacheWriteTokens: 4,
		ReasoningTokens:  5,
	})
	if err != nil {
		t.Fatalf("编码：%v", err)
	}
	var keys map[string]int64
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("解码：%v", err)
	}
	want := map[string]int64{
		"input_tokens":       1,
		"output_tokens":      2,
		"cache_read_tokens":  3,
		"cache_write_tokens": 4,
		"reasoning_tokens":   5,
	}
	if len(keys) != len(want) {
		t.Errorf("线上键 = %v，want 恰好 %d 个：%v", keys, len(want), want)
	}
	for k, v := range want {
		got, ok := keys[k]
		if !ok {
			t.Errorf("线上少了键 %q；调度层那一列会永远为空", k)
			continue
		}
		if got != v {
			t.Errorf("键 %q = %d，want %d（字段与键对错了）", k, got, v)
		}
	}
}
