package responses

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 本文件覆盖 R110（旧仓 #80 / 109ff61）在 responses 一族的响应侧保真：
// steered 独立停止档、completed_at / prompt_cache_diagnostics / moderation
// 三项响应专属回执的同族往返、reasoning content 通道与 summary 通道的分离。
// 这些位的共同点是「上游给了而代理记零」——不落 IR 就等于全线静默丢失。
// 旧仓同点的 gemini 服务端工具历史（item 10）与 anthropic 响应侧附件跳过
//（item 9）不在本仓范围：gemini 纯出站无入站解码路径，附件跳过旧仓另列。

// ---- steered 独立停止档 ----

// responses 的 incomplete_details.reason="steered"（用户中途转向在安全边界处
// 截断）必须成独立档，且与 max_tokens 分开：那是输出配额耗尽要加大预算重试，
// steered 加大预算毫无意义。塌成 max_tokens 会让客户端误判补救动作。
func TestR110SteeredIsDistinctIncompleteReason(t *testing.T) {
	body := []byte(`{"id":"resp_1","model":"m","status":"incomplete","output":[],` +
		`"incomplete_details":{"reason":"steered"}}`)
	resp, err := DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.StopReason != ir.StopSteered {
		t.Fatalf("StopReason = %q, want steered（被塌成了别的档）", resp.StopReason)
	}
}

// steered 同族回写原值，不塌成 max_output_tokens。
func TestR110SteeredEncodesBackVerbatim(t *testing.T) {
	out, err := EncodeResponse(&ir.Response{
		ID: "resp_1", Model: "m", StopReason: ir.StopSteered,
		Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}},
	})
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	var got struct {
		Status            string `json:"status"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if got.Status != "incomplete" || got.IncompleteDetails == nil || got.IncompleteDetails.Reason != "steered" {
		t.Errorf("steered 回写丢档：status=%q details=%+v\n%s", got.Status, got.IncompleteDetails, out)
	}
}

// renderStatus 的 steered 档：与 context_window 归 max_output_tokens 不同，
// steered 有专属 reason 取值，原样回写。
func TestR110SteeredRenderStatus(t *testing.T) {
	status, incomplete := renderStatus(ir.StopSteered)
	if status != "incomplete" || incomplete == nil || incomplete.Reason != "steered" {
		t.Fatalf("renderStatus(steered) = %q, %+v", status, incomplete)
	}
}

// 流式：response.incomplete 帧带 incomplete_details.reason=steered 要解成
// StopSteered 挂上终止 delta。
func TestR110SteeredStreamDecode(t *testing.T) {
	d := newStreamDecoder()
	if _, err := d.Feed("", `{"type":"response.created","response":{"id":"r1","model":"m"}}`); err != nil {
		t.Fatal(err)
	}
	evs, err := d.Feed("",
		`{"type":"response.incomplete","response":{"id":"r1","status":"incomplete","incomplete_details":{"reason":"steered"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	var got ir.StopReason
	for _, ev := range evs {
		if ev.Type == ir.EvMessageDelta {
			got = ev.StopReason
		}
	}
	if got != ir.StopSteered {
		t.Fatalf("流式 steered = %q, want steered", got)
	}
}

// ---- prompt_cache_diagnostics 同族往返 ----

// 官方 response.prompt_cache_diagnostics（cache_hit/cache_miss/... 判别式联合）
// 是响应专属回执：同族往返必须逐字带回，显式 null 不当成有回执。
func TestR110PromptCacheDiagnosticsRoundTrip(t *testing.T) {
	body := []byte(`{"id":"r1","model":"m","status":"completed","output":[],` +
		`"prompt_cache_diagnostics":{"type":"cache_hit","cached_tokens":128}}`)
	resp, err := DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(resp.ResponsesPromptCacheDiagnostics) == 0 {
		t.Fatal("prompt_cache_diagnostics 没落进 IR")
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if !strings.Contains(string(out), `"prompt_cache_diagnostics"`) ||
		!strings.Contains(string(out), `"cache_hit"`) {
		t.Errorf("同族往返丢了 prompt_cache_diagnostics：%s", out)
	}
}

// 显式 null 等同没给：不把 4 字节字面量当成有回执写回。
func TestR110PromptCacheDiagnosticsNullNotAReceipt(t *testing.T) {
	resp, err := DecodeResponse([]byte(
		`{"id":"r1","model":"m","status":"completed","output":[],"prompt_cache_diagnostics":null}`))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(resp.ResponsesPromptCacheDiagnostics) != 0 {
		t.Fatalf("null 被当成了回执：%s", resp.ResponsesPromptCacheDiagnostics)
	}
	out, _ := EncodeResponse(resp)
	if strings.Contains(string(out), "prompt_cache_diagnostics") {
		t.Errorf("null 不该写出键：%s", out)
	}
}

// 流式：终止帧的 response.prompt_cache_diagnostics 经 EvMessageDelta 进聚合器。
func TestR110PromptCacheDiagnosticsStreamRoundTrip(t *testing.T) {
	d := newStreamDecoder()
	if _, err := d.Feed("", `{"type":"response.created","response":{"id":"r1","model":"m"}}`); err != nil {
		t.Fatal(err)
	}
	evs, err := d.Feed("", `{"type":"response.completed","response":{"id":"r1","status":"completed",`+
		`"prompt_cache_diagnostics":{"type":"cache_miss"},"moderation":{"output_flagged":true},"completed_at":1700000999}}`)
	if err != nil {
		t.Fatal(err)
	}
	var delta *ir.Event
	for i := range evs {
		if evs[i].Type == ir.EvMessageDelta {
			delta = &evs[i]
		}
	}
	if delta == nil {
		t.Fatal("没编出终止 delta")
	}
	if !strings.Contains(string(delta.PromptCacheDiagnostics), "cache_miss") {
		t.Errorf("delta 丢了 prompt_cache_diagnostics：%s", delta.PromptCacheDiagnostics)
	}
	if !strings.Contains(string(delta.Moderation), "output_flagged") {
		t.Errorf("delta 丢了 moderation：%s", delta.Moderation)
	}
	if delta.CompletedAt != 1700000999 {
		t.Errorf("delta 丢了 completed_at：%d", delta.CompletedAt)
	}
}

// ---- moderation 同族往返 ----

func TestR110ModerationRoundTrip(t *testing.T) {
	body := []byte(`{"id":"r1","model":"m","status":"completed","output":[],` +
		`"moderation":{"input_flagged":false,"output_flagged":true}}`)
	resp, err := DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(resp.ResponsesModeration) == 0 {
		t.Fatal("moderation 没落进 IR")
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if !strings.Contains(string(out), `"moderation"`) ||
		!strings.Contains(string(out), `"output_flagged":true`) {
		t.Errorf("同族往返丢了 moderation：%s", out)
	}
}

func TestR110ModerationNullNotAReceipt(t *testing.T) {
	resp, err := DecodeResponse([]byte(
		`{"id":"r1","model":"m","status":"completed","output":[],"moderation":null}`))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(resp.ResponsesModeration) != 0 {
		t.Fatalf("null 被当成了回执：%s", resp.ResponsesModeration)
	}
	out, _ := EncodeResponse(resp)
	if strings.Contains(string(out), "moderation") {
		t.Errorf("null 不该写出键：%s", out)
	}
}

// ---- completed_at 同族往返（响应专属，不拿本地钟伪造）----

func TestR110CompletedAtRoundTrip(t *testing.T) {
	resp, err := DecodeResponse([]byte(
		`{"id":"r1","model":"m","status":"completed","output":[],"created_at":1700000000,"completed_at":1700000999}`))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.CompletedAt != 1700000999 {
		t.Fatalf("completed_at 没落进 IR：%d", resp.CompletedAt)
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if !strings.Contains(string(out), `"completed_at":1700000999`) {
		t.Errorf("同族往返丢了 completed_at：%s", out)
	}
}

// 上游没给完成时间（零值）时绝不伪造一个本地完成时间：键整个不出现。
// 与 created_at 的本地钟回退刻意不同——created 是「请求何时开始」的近似量，
// completed 是「生成何时结束」的上游事实，编造它是无中生有。
func TestR110CompletedAtZeroOmitted(t *testing.T) {
	resp, err := DecodeResponse([]byte(`{"id":"r1","model":"m","status":"completed","output":[]}`))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	out, _ := EncodeResponse(resp)
	if strings.Contains(string(out), "completed_at") {
		t.Errorf("零值 completed_at 不该写出（伪造完成时间）：%s", out)
	}
}

// ---- reasoning content 通道保真 ----

// 官方 reasoning item 有两条独立通道：summary（reasoning_summary_text，摘要）
// 与 content（reasoning_text，加密推理原文）。summary 为空、正文在 content 时，
// 解码必须标记 ContentChannel，否则同族回写会把 content 原文塌进 summary 通道。
func TestR110ReasoningContentChannelDecode(t *testing.T) {
	body := []byte(`{"id":"r1","model":"m","status":"completed","output":[` +
		`{"type":"reasoning","id":"rs_1","summary":[],"content":[{"type":"reasoning_text","text":"INNER"}]}` +
		`]}`)
	resp, err := DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	var th *ir.Thinking
	for _, b := range resp.Content {
		if b.Type == ir.BlockThinking && b.Thinking != nil {
			th = b.Thinking
		}
	}
	if th == nil {
		t.Fatalf("content 通道的推理没解出思考块：%+v", resp.Content)
	}
	if th.Text != "INNER" {
		t.Errorf("正文 = %q, want INNER", th.Text)
	}
	if !th.ContentChannel {
		t.Error("走 content 通道却没标记 ContentChannel，同族回写会塌进 summary")
	}
}

// summary 通道（默认）不标记 ContentChannel：维持历史行为。
func TestR110ReasoningSummaryChannelNotMarked(t *testing.T) {
	body := []byte(`{"id":"r1","model":"m","status":"completed","output":[` +
		`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"SUMM"}]}` +
		`]}`)
	resp, err := DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	for _, b := range resp.Content {
		if b.Type == ir.BlockThinking && b.Thinking != nil {
			if b.Thinking.ContentChannel {
				t.Error("summary 通道被误标成 ContentChannel")
			}
			if b.Thinking.Text != "SUMM" {
				t.Errorf("正文 = %q, want SUMM", b.Thinking.Text)
			}
			return
		}
	}
	t.Fatalf("summary 通道没解出思考块：%+v", resp.Content)
}

// 编码回写：ContentChannel=true 写 content 数组（reasoning_text）+ 空 summary
// 占位（官方 summary required）；false 写 summary_text。
func TestR110ReasoningContentChannelEncode(t *testing.T) {
	enc := func(cc bool) string {
		out, err := EncodeResponse(&ir.Response{
			ID: "r1", Model: "m", StopReason: ir.StopEndTurn,
			Content: []ir.Block{{Type: ir.BlockThinking,
				Thinking: &ir.Thinking{Text: "INNER", ContentChannel: cc, ItemID: "rs_1"}}},
		})
		if err != nil {
			t.Fatalf("EncodeResponse: %v", err)
		}
		return string(out)
	}
	content := enc(true)
	if !strings.Contains(content, `"reasoning_text"`) || !strings.Contains(content, `"INNER"`) {
		t.Errorf("ContentChannel=true 没写回 content 通道：%s", content)
	}
	if strings.Contains(content, `"summary_text"`) {
		t.Errorf("ContentChannel=true 不该把原文塞进 summary：%s", content)
	}
	// summary 官方 required：content 通道下写空数组占位，而不是省略整个键。
	if !strings.Contains(content, `"summary":[]`) {
		t.Errorf("ContentChannel=true 应写空 summary 占位：%s", content)
	}
	summary := enc(false)
	if !strings.Contains(summary, `"summary_text"`) || strings.Contains(summary, `"reasoning_text"`) {
		t.Errorf("ContentChannel=false 应走 summary 通道：%s", summary)
	}
}

// 防御性断言：wireResponse 的三项新回执字段都带 omitempty。Clone 与投影走
// JSON 往返，缺 omitempty 的 RawMessage 会从「空」变成 4 字节 null 字面量，
// completed_at 的 0 会写出一个假的完成时间。
func TestR110WireResponseOmemptypresent(t *testing.T) {
	raw, _ := json.Marshal(wireResponse{ID: "r1", Model: "m"})
	s := string(raw)
	for _, k := range []string{"prompt_cache_diagnostics", "moderation", "completed_at"} {
		if strings.Contains(s, k) {
			t.Errorf("空 wireResponse 不该写出 %s（缺 omitempty）：%s", k, s)
		}
	}
}
