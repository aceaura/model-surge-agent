package chatcompletions

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// #70 的 chat 专属回执：system_fingerprint（后端配置指纹，排障信号）与
// usage 的音频/预测四位细分。指纹与档位同规律：chunk 顶层、可能晚到，
// 解码侧攒着随收尾帧交付，编码侧一旦收到就逐帧回显。

// 流式：首帧就带指纹时，start 事件与收尾 delta 都要交付。
func TestStreamFingerprintOnStartAndFinish(t *testing.T) {
	dec := newStreamDecoder()
	frames := []string{
		`{"id":"c1","model":"m","system_fingerprint":"fp_1","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`,
		doneSentinel,
	}
	var start, delta *ir.Event
	for _, f := range frames {
		evs, err := dec.Feed("", f)
		if err != nil {
			t.Fatalf("feed %s: %v", f, err)
		}
		for i := range evs {
			switch evs[i].Type {
			case ir.EvMessageStart:
				start = &evs[i]
			case ir.EvMessageDelta:
				delta = &evs[i]
			}
		}
	}
	if start == nil || start.SystemFingerprint != "fp_1" {
		t.Fatalf("start 事件没带指纹：%+v", start)
	}
	if delta == nil || delta.SystemFingerprint != "fp_1" {
		t.Errorf("收尾 delta 没带指纹：%+v", delta)
	}
}

// 晚到的指纹也要交付：上游可能只在中间某帧带 system_fingerprint，
// 只认首帧会让「有指纹」静默变「没指纹」。
func TestStreamFingerprintLateArrival(t *testing.T) {
	dec := newStreamDecoder()
	frames := []string{
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"a"}}]}`,
		`{"id":"c1","model":"m","system_fingerprint":"fp_late","choices":[{"index":0,"delta":{"content":"b"}}]}`,
		doneSentinel,
	}
	var delta *ir.Event
	for _, f := range frames {
		evs, err := dec.Feed("", f)
		if err != nil {
			t.Fatalf("feed %s: %v", f, err)
		}
		for i := range evs {
			if evs[i].Type == ir.EvMessageDelta {
				delta = &evs[i]
			}
		}
	}
	if delta == nil || delta.SystemFingerprint != "fp_late" {
		t.Fatalf("晚到的指纹没随收尾帧交付：%+v", delta)
	}
}

// 编码侧：收到指纹后逐帧回显；只在 delta 上晚到也补得上（与档位同型）。
func TestEncoderEchoesFingerprintOnEveryChunk(t *testing.T) {
	enc := newStreamEncoder()
	var joined []byte
	appendFrames := func(frames [][]byte, err error, what string) {
		t.Helper()
		if err != nil {
			t.Fatalf("Encode(%s): %v", what, err)
		}
		for _, f := range frames {
			joined = append(joined, f...)
		}
	}
	f1, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "c1", Model: "m"})
	appendFrames(f1, err, "start")
	// 指纹随 delta 晚到。
	f2, err := enc.Encode(ir.Event{Type: ir.EvMessageDelta, SystemFingerprint: "fp_2"})
	appendFrames(f2, err, "delta")
	f3, err := enc.Encode(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "hi"})
	appendFrames(f3, err, "content")
	appendFrames(enc.Finish(), nil, "finish")

	if n := strings.Count(string(joined), `"system_fingerprint":"fp_2"`); n == 0 {
		t.Errorf("晚到的指纹没被回显：%s", joined)
	}
}

// 非流式往返：system_fingerprint 原值进 IR、原值写出；异族来源恒为空，
// omitempty 自然不带。
func TestWholeResponseFingerprintRoundTrip(t *testing.T) {
	body := []byte(`{"id":"c1","object":"chat.completion","created":1700000000,"model":"m",` +
		`"system_fingerprint":"fp_9",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	resp, _, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if resp.SystemFingerprint != "fp_9" {
		t.Fatalf("指纹没落进 IR：%q", resp.SystemFingerprint)
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if !strings.Contains(string(out), `"system_fingerprint":"fp_9"`) {
		t.Errorf("出站丢了指纹：%s", out)
	}

	// 空指纹不凭空造键。
	out, err = EncodeResponse(&ir.Response{ID: "c2", Model: "m", StopReason: ir.StopEndTurn})
	if err != nil {
		t.Fatalf("EncodeResponse(empty): %v", err)
	}
	if strings.Contains(string(out), "system_fingerprint") {
		t.Errorf("空指纹被凭空写出：%s", out)
	}
}

// usage 的音频/预测四位细分：解码进 IR，编码原样回写。
// 四位都是「客户端已收到而代理记零」型维度，少一位账就少一位。
func TestUsageAudioAndPredictionDetailsRoundTrip(t *testing.T) {
	body := []byte(`{"id":"c1","object":"chat.completion","created":1700000000,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,` +
		`"prompt_tokens_details":{"cached_tokens":10,"audio_tokens":20},` +
		`"completion_tokens_details":{"reasoning_tokens":5,"audio_tokens":30,` +
		`"accepted_prediction_tokens":40,"rejected_prediction_tokens":7}}}`)
	resp, _, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	u := resp.Usage
	if u.PromptAudioTokens != 20 || u.CompletionAudioTokens != 30 ||
		u.AcceptedPredictionTokens != 40 || u.RejectedPredictionTokens != 7 {
		t.Fatalf("四位细分没进 IR：%+v", u)
	}
	if u.CacheReadTokens != 10 {
		t.Errorf("原有的 cached_tokens 被改坏：%d", u.CacheReadTokens)
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	for _, want := range []string{
		`"audio_tokens":20`, `"audio_tokens":30`,
		`"accepted_prediction_tokens":40`, `"rejected_prediction_tokens":7`,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("出站缺 %s：%s", want, out)
		}
	}
}

// 四位全零时不凭空造 details 对象。
func TestZeroUsageDetailsStayAbsent(t *testing.T) {
	out, err := EncodeResponse(&ir.Response{ID: "c1", Model: "m", StopReason: ir.StopEndTurn,
		Usage: ir.Usage{InputTokens: 3, OutputTokens: 4}})
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	for _, banned := range []string{"prompt_tokens_details", "completion_tokens_details"} {
		if strings.Contains(string(out), banned) {
			t.Errorf("零值细分被凭空写出 %s：%s", banned, out)
		}
	}
}
