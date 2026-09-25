package ir

import (
	"encoding/json"
	"strings"
	"testing"
)

// 本文件覆盖 R110（旧仓 #80 / 109ff61）在 IR 层的四件事：steered 独立停止档、
// usage.iterations 合并语义、responses 三项响应侧回执经「投影→聚合」往返存活、
// reasoning ContentChannel 标记熬过 Clone。回执走事件流是本仓的硬约束：所有
// 响应（含非流式上游）都经 ResponseEvents 重放 + Aggregator 还原，漏投影或漏
// 聚合等于「流式不丢、非流式丢」的分叉。

// steered 是独立档，取值就是官方 incomplete_details.reason 的字面量。
func TestR110StopSteeredIsDistinct(t *testing.T) {
	if StopSteered != "steered" {
		t.Errorf("StopSteered = %q, want steered", StopSteered)
	}
	if StopSteered == StopMaxTokens {
		t.Error("steered 塌进了 max_tokens：两者补救动作相反（转向 vs 加预算）")
	}
}

// usage.iterations 是原文透传维度：非空后到者胜，空不抹已有值——与其余
// 非零覆盖维度同规律。
func TestR110MergeUsageCarriesIterations(t *testing.T) {
	var into Usage
	MergeUsage(&into, Usage{Iterations: json.RawMessage(`{"message":{"input_tokens":1}}`)})
	if !strings.Contains(string(into.Iterations), "message") {
		t.Fatalf("首帧 iterations 没落下：%s", into.Iterations)
	}
	// 空帧不抹掉已有值。
	MergeUsage(&into, Usage{InputTokens: 5})
	if !strings.Contains(string(into.Iterations), "message") {
		t.Errorf("空 iterations 把已有值抹掉了：%s", into.Iterations)
	}
	// 后到的非空覆盖。
	MergeUsage(&into, Usage{Iterations: json.RawMessage(`{"compaction":{"input_tokens":3}}`)})
	if !strings.Contains(string(into.Iterations), "compaction") {
		t.Errorf("后到的 iterations 没胜出：%s", into.Iterations)
	}
}

// 三项 responses 响应侧回执经「整份投影 → 聚合还原」往返回来：这是非流式
// 上游走的生产路径，任何一项漏投影或聚合器漏收都会在还原时归零。
func TestR110ResponseReceiptsSurviveReplayAggregate(t *testing.T) {
	resp := &Response{ID: "r1", Model: "m",
		CompletedAt:                     1700000999,
		ResponsesPromptCacheDiagnostics: json.RawMessage(`{"type":"cache_hit","cached_tokens":128}`),
		ResponsesModeration:             json.RawMessage(`{"output_flagged":true}`)}
	evs := ResponseEvents(resp)
	var a Aggregator
	for _, ev := range evs {
		a.Add(ev)
	}
	got := a.Response()
	if got.CompletedAt != 1700000999 {
		t.Errorf("completed_at 往返丢值：%d", got.CompletedAt)
	}
	if !strings.Contains(string(got.ResponsesPromptCacheDiagnostics), "cache_hit") {
		t.Errorf("prompt_cache_diagnostics 往返丢失：%s", got.ResponsesPromptCacheDiagnostics)
	}
	if !strings.Contains(string(got.ResponsesModeration), "output_flagged") {
		t.Errorf("moderation 往返丢失：%s", got.ResponsesModeration)
	}
}

// 聚合器对回执「两处都收」：终止 delta 才带来时（真流式）也要收下，不能只
// 认 start。后值覆盖，与 created/service_tier 同口径。
func TestR110AggregatorPicksReceiptsFromDeltaToo(t *testing.T) {
	var a Aggregator
	a.Add(Event{Type: EvMessageStart})
	a.Add(Event{Type: EvMessageDelta, CompletedAt: 1700000500,
		PromptCacheDiagnostics: json.RawMessage(`{"type":"cache_miss"}`),
		Moderation:             json.RawMessage(`{"input_flagged":true}`)})
	got := a.Response()
	if got.CompletedAt != 1700000500 {
		t.Errorf("delta 上的 completed_at 没收下：%d", got.CompletedAt)
	}
	if !strings.Contains(string(got.ResponsesPromptCacheDiagnostics), "cache_miss") {
		t.Errorf("delta 上的诊断没收下：%s", got.ResponsesPromptCacheDiagnostics)
	}
	if !strings.Contains(string(got.ResponsesModeration), "input_flagged") {
		t.Errorf("delta 上的审核没收下：%s", got.ResponsesModeration)
	}
}

// reasoning 的 ContentChannel 标记必须熬过 Clone：请求侧编码会 Clone 整份
// 请求，标记丢了同族回写就会把 content 原文塌进 summary 通道。
func TestR110ContentChannelSurvivesClone(t *testing.T) {
	req := &Request{Messages: []Message{{Role: RoleAssistant,
		Content: []Block{{Type: BlockThinking,
			Thinking: &Thinking{Text: "INNER", ContentChannel: true}}}}}}
	cloned := req.Clone()
	th := cloned.Messages[0].Content[0].Thinking
	if th == nil || !th.ContentChannel {
		t.Fatalf("ContentChannel 没熬过 Clone：%+v", th)
	}
	// 深拷贝：改克隆体不得回写原体。
	th.ContentChannel = false
	if !req.Messages[0].Content[0].Thinking.ContentChannel {
		t.Error("Clone 是浅拷贝：改克隆体污染了原请求")
	}
}
