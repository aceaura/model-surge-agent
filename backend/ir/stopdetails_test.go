package ir

import (
	"reflect"
	"testing"
)

// #70 的 IR 层三件事：usage 六维新增与 inference_geo 的合并语义、
// stop_details / system_fingerprint 走聚合器、整份投影把它们送回事件流。
// 这几位都是「上游给了而代理记零」型缺口：不落 IR 就等于全线静默丢失。

// 六维新增与其余非零后到者胜的维度同规律：流式的用量分帧到达，
// 首帧常带全量、末帧只带增量，任何一位粘住零值都会少记。
func TestMergeUsageCarriesNewDimensions(t *testing.T) {
	var into Usage
	MergeUsage(&into, Usage{WebSearchRequests: 1, WebFetchRequests: 2,
		PromptAudioTokens: 3, CompletionAudioTokens: 4,
		AcceptedPredictionTokens: 5, RejectedPredictionTokens: 6})
	// 后到的非零覆盖，零不动已有值。
	MergeUsage(&into, Usage{WebSearchRequests: 9, CompletionAudioTokens: 0})
	want := Usage{WebSearchRequests: 9, WebFetchRequests: 2,
		PromptAudioTokens: 3, CompletionAudioTokens: 4,
		AcceptedPredictionTokens: 5, RejectedPredictionTokens: 6}
	if into != want {
		t.Errorf("合并后 = %+v，want %+v", into, want)
	}
}

// inference_geo 是字符串维度：非空后到者胜，空串不许把已有值抹掉。
func TestMergeUsageCarriesInferenceGeo(t *testing.T) {
	var into Usage
	MergeUsage(&into, Usage{InferenceGeo: "us"})
	MergeUsage(&into, Usage{InferenceGeo: ""})
	if into.InferenceGeo != "us" {
		t.Errorf("空串把 geo 抹掉了：%q", into.InferenceGeo)
	}
	MergeUsage(&into, Usage{InferenceGeo: "eu"})
	if into.InferenceGeo != "eu" {
		t.Errorf("后到的非空没胜出：%q", into.InferenceGeo)
	}
}

// 指纹与档位同规律：start 事件收下，晚到（delta 才带来）的也要收下——
// chat 上游可能把 system_fingerprint 挂在任意一帧 chunk 顶层。
func TestAggregatorPicksSystemFingerprintFromBothEvents(t *testing.T) {
	var a Aggregator
	a.Add(Event{Type: EvMessageStart, SystemFingerprint: "fp_start"})
	if got := a.Response().SystemFingerprint; got != "fp_start" {
		t.Fatalf("start 上的指纹没收下：%q", got)
	}
	a.Add(Event{Type: EvMessageDelta, SystemFingerprint: "fp_late"})
	if got := a.Response().SystemFingerprint; got != "fp_late" {
		t.Errorf("晚到的指纹没补上：%q", got)
	}
}

// stop_details 只随终止 delta 到达，聚合器要把它挂上响应。
func TestAggregatorPicksStopDetails(t *testing.T) {
	var a Aggregator
	a.Add(Event{Type: EvMessageStart})
	sd := &StopDetails{Category: "refusal_other", Explanation: "policy"}
	a.Add(Event{Type: EvMessageDelta, StopReason: StopReason("refusal"), StopDetails: sd})
	got := a.Response().StopDetails
	if got == nil || *got != *sd {
		t.Fatalf("stop_details 没进响应：%+v", got)
	}
}

// 整份响应投影回事件流时必须带上这两位：非流式上游也要走同一条
// 聚合路径，漏投影等于「非流式丢、流式不丢」的分叉。
func TestResponseEventsProjectsStopDetailsAndFingerprint(t *testing.T) {
	sd := &StopDetails{Category: "refusal_other", Explanation: "why"}
	resp := &Response{ID: "msg_1", Model: "m", StopReason: StopReason("refusal"),
		StopDetails: sd, SystemFingerprint: "fp_1"}
	evs := ResponseEvents(resp)
	if len(evs) < 2 {
		t.Fatalf("事件数 = %d", len(evs))
	}
	if evs[0].Type != EvMessageStart || evs[0].SystemFingerprint != "fp_1" {
		t.Errorf("start 事件没带指纹：%+v", evs[0])
	}
	var delta *Event
	for i := range evs {
		if evs[i].Type == EvMessageDelta {
			delta = &evs[i]
		}
	}
	if delta == nil || delta.StopDetails == nil || !reflect.DeepEqual(delta.StopDetails, sd) {
		t.Errorf("delta 事件没带 stop_details：%+v", delta)
	}
	// 聚合回整份响应应当还原两位。
	var a Aggregator
	for _, ev := range evs {
		a.Add(ev)
	}
	got := a.Response()
	if got.SystemFingerprint != "fp_1" || got.StopDetails == nil || *got.StopDetails != *sd {
		t.Errorf("往返回丢：指纹 %q stop_details %+v", got.SystemFingerprint, got.StopDetails)
	}
}
