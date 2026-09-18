package gemini

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 这里测本协议独有的形态：模型名进路径、name 回指、thought 标记、
// 推理用量并入输出、无终止标记。跨协议矩阵只验语义抵达，测不到这些。

func TestEndpointPutsModelInPathAndPicksMethodByStream(t *testing.T) {
	c := outboundCodec{}
	got, _ := c.Endpoint("https://host/v1beta/", "gemini-3-pro", true)
	want := "https://host/v1beta/models/gemini-3-pro:streamGenerateContent?alt=sse"
	if got != want {
		t.Errorf("stream endpoint = %q, want %q", got, want)
	}
	got, _ = c.Endpoint("https://host/v1beta", "gemini-3-pro", false)
	want = "https://host/v1beta/models/gemini-3-pro:generateContent"
	if got != want {
		t.Errorf("non-stream endpoint = %q, want %q", got, want)
	}
}

func TestRequestBodyCarriesNoModelOrStreamField(t *testing.T) {
	// 这两个字段由 Endpoint 表达。写进体里上游会当成未知字段，
	// 严格模式下直接 400。
	body, err := EncodeRequest(&ir.Request{
		Model:    "gemini-3-pro",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"model", "stream"} {
		if _, ok := m[key]; ok {
			t.Errorf("body must not carry %q: %s", key, body)
		}
	}
}

func TestSystemGoesToSystemInstructionAndRolesAreUserOrModel(t *testing.T) {
	body, err := EncodeRequest(&ir.Request{
		Model:  "m",
		System: []ir.Block{{Type: ir.BlockText, Text: "be terse"}},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
			{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: "yo"}}},
		},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var w wireRequest
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if w.SystemInstruction == nil || w.SystemInstruction.Parts[0].Text != "be terse" {
		t.Fatalf("systemInstruction = %+v, want the system text", w.SystemInstruction)
	}
	// 本协议没有 system 与 assistant 两个角色名。
	if got := []string{w.Contents[0].Role, w.Contents[1].Role}; got[0] != roleUser || got[1] != roleModel {
		t.Errorf("roles = %v, want [user model]", got)
	}
}

func TestToolResultReferencesTheCallByName(t *testing.T) {
	// 本协议的 functionResponse 靠 name 回指，不认 id。翻不出 name
	// 上游会因为对不上调用而拒绝整个请求。
	body, err := EncodeRequest(&ir.Request{
		Model: "m",
		Messages: []ir.Message{
			{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
				ID: "call_1", Name: "grep", Input: `{"pattern":"TODO"}`,
			}}}},
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
				ToolUseID: "call_1",
				Content:   []ir.Block{{Type: ir.BlockText, Text: "found 3"}},
			}}}},
		},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var w wireRequest
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var resp *wireFunctionResp
	for _, c := range w.Contents {
		for _, p := range c.Parts {
			if p.FunctionResponse != nil {
				resp = p.FunctionResponse
			}
		}
	}
	if resp == nil {
		t.Fatalf("no functionResponse in body: %s", body)
	}
	if resp.Name != "grep" {
		t.Errorf("functionResponse name = %q, want grep", resp.Name)
	}
	// response 必须是对象，裸字符串会被拒。
	var payload map[string]string
	if err := json.Unmarshal(resp.Response, &payload); err != nil {
		t.Fatalf("response must be an object: %s", resp.Response)
	}
	if payload["output"] != "found 3" {
		t.Errorf("response = %v, want output=found 3", payload)
	}
}

func TestThinkingBecomesThoughtFlaggedTextAndForeignSignatureIsDropped(t *testing.T) {
	body, err := EncodeRequest(&ir.Request{
		Model:    "m",
		Thinking: &ir.ThinkingConfig{Enabled: ir.ThinkingOn(), Effort: "high"},
		Messages: []ir.Message{{Role: ir.RoleAssistant, Content: []ir.Block{
			{Type: ir.BlockThinking, Thinking: &ir.Thinking{
				Text: "pondering", Signature: "foreign-sig", SignatureFrom: codec.ProtocolAnthropic,
			}},
			{Type: ir.BlockThinking, Thinking: &ir.Thinking{
				Text: "more", Signature: "own-sig", SignatureFrom: Name,
			}},
		}}},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if strings.Contains(string(body), "foreign-sig") {
		t.Errorf("a foreign signature leaked into the body: %s", body)
	}
	if !strings.Contains(string(body), "own-sig") {
		t.Errorf("own signature must survive: %s", body)
	}
	var w wireRequest
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if w.GenerationConfig == nil || w.GenerationConfig.ThinkingConfig == nil {
		t.Fatalf("thinkingConfig missing: %s", body)
	}
	// 不显式要 includeThoughts 就收不到推理内容。
	if !w.GenerationConfig.ThinkingConfig.IncludeThoughts {
		t.Error("includeThoughts must be true or the upstream returns no reasoning")
	}
	var thoughts int
	for _, p := range w.Contents[0].Parts {
		if p.Thought {
			thoughts++
		}
	}
	// 同一消息里的多个推理块都要留下，只留最后一个会丢内容。
	if thoughts != 2 {
		t.Errorf("thought parts = %d, want 2: %s", thoughts, body)
	}
}

func TestToolChoiceToolBecomesAnyWithAllowList(t *testing.T) {
	// 本协议没有「必须用这一个工具」的模式。
	body, err := EncodeRequest(&ir.Request{
		Model:      "m",
		ToolChoice: &ir.ToolChoice{Mode: ir.ToolChoiceTool, Name: "grep"},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var w wireRequest
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cfg := w.ToolConfig.FunctionCallingConfig
	if cfg.Mode != modeAny || len(cfg.AllowedFunctionNames) != 1 || cfg.AllowedFunctionNames[0] != "grep" {
		t.Errorf("functionCallingConfig = %+v, want ANY with [grep]", cfg)
	}
}

// gemini 的流每帧都是完整响应对象，parts 是隐式续写，且没有终止标记。
const streamRaw = `data: {"responseId":"r1","modelVersion":"native","candidates":[{"content":{"role":"model","parts":[{"text":"pondering","thought":true}]}}],"usageMetadata":{"promptTokenCount":120,"cachedContentTokenCount":30}}

data: {"candidates":[{"content":{"role":"model","parts":[{"text":"search"},{"text":"ing"}]}}]}

data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"grep","args":{"pattern":"TODO"}}}]}}],"usageMetadata":{"promptTokenCount":120,"candidatesTokenCount":40,"thoughtsTokenCount":5,"cachedContentTokenCount":30}}

data: {"candidates":[{"finishReason":"STOP","index":0}]}

`

func TestStreamSplitsBlocksWhenPartKindChanges(t *testing.T) {
	resp := aggregateStream(t, streamRaw)

	var (
		text     string
		thinking string
		use      *ir.ToolUse
	)
	for _, b := range resp.Content {
		switch b.Type {
		case ir.BlockText:
			text += b.Text
		case ir.BlockThinking:
			thinking += b.Thinking.Text
		case ir.BlockToolUse:
			use = b.ToolUse
		}
	}
	if thinking != "pondering" {
		t.Errorf("thinking = %q, want pondering", thinking)
	}
	// 同种类的相邻 parts 必须续写进同一块，另起一块会让下游看到两段文本。
	if text != "searching" {
		t.Errorf("text = %q, want searching", text)
	}
	if use == nil {
		t.Fatalf("no tool_use block: %+v", resp.Content)
	}
	if use.Name != "grep" {
		t.Errorf("tool name = %q, want grep", use.Name)
	}
	// 上游没给 id 时必须合成一个，否则下游三协议都无法把结果回指到调用。
	if use.ID == "" {
		t.Error("a call id must be synthesized when the upstream gives none")
	}
}

func TestStreamFoldsThoughtTokensIntoOutputAndInfersToolUse(t *testing.T) {
	resp := aggregateStream(t, streamRaw)

	// 推理消耗不含在 candidatesTokenCount 里，但计费上属于输出，
	// 所以既并进输出总量（40+5=45），又单记一维供成本归因。
	// promptTokenCount 120 含 cachedContentTokenCount 30，
	// 而 IR 的 InputTokens 是不含缓存的新鲜输入，故为 90。
	want := ir.Usage{InputTokens: 90, OutputTokens: 45, CacheReadTokens: 30, ReasoningTokens: 5}
	if resp.Usage != want {
		t.Errorf("usage = %+v, want %+v", resp.Usage, want)
	}
	// 本协议以工具调用收尾时仍报 STOP。照直翻成 end_turn 会让客户端
	// 以为回合结束而不去执行工具。
	if resp.StopReason != ir.StopToolUse {
		t.Errorf("stop_reason = %q, want tool_use", resp.StopReason)
	}
}

func TestFinishSynthesizesTerminationBecauseThereIsNoTerminalFrame(t *testing.T) {
	dec := newStreamDecoder()
	events, err := dec.Feed("", `{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`)
	if err != nil {
		t.Fatalf("feed: %v", err)
	}
	for _, ev := range events {
		if ev.Type == ir.EvMessageStop {
			t.Fatal("the protocol has no terminal frame; termination must come from Finish")
		}
	}
	var kinds []ir.EventType
	for _, ev := range dec.Finish() {
		kinds = append(kinds, ev.Type)
	}
	want := []ir.EventType{ir.EvBlockStop, ir.EvMessageDelta, ir.EvMessageStop}
	if len(kinds) != len(want) {
		t.Fatalf("finish events = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("finish events = %v, want %v", kinds, want)
		}
	}
}

func TestStreamErrorFrameWithoutStatusCodeIsClassifiedByStatusString(t *testing.T) {
	// 流内错误帧没有 HTTP 状态码，限流只能靠 RESOURCE_EXHAUSTED 识别；
	// 归错类会让调度层不去换目标。
	dec := newStreamDecoder()
	events, err := dec.Feed("", `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"quota"}}`)
	if err != nil {
		t.Fatalf("feed: %v", err)
	}
	if len(events) != 1 || events[0].Type != ir.EvError {
		t.Fatalf("events = %+v, want one error event", events)
	}
	if got := events[0].Err; got.Kind != ir.ErrRateLimit || !got.Retryable {
		t.Errorf("err = %+v, want retryable rate_limit", got)
	}
}

func TestSafetyBlockOnThePromptSurfacesAsContentFilter(t *testing.T) {
	// 整个请求被安全策略拒时 candidates 为空，原因只在 promptFeedback 里。
	dec := newStreamDecoder()
	if _, err := dec.Feed("", `{"promptFeedback":{"blockReason":"SAFETY"}}`); err != nil {
		t.Fatalf("feed: %v", err)
	}
	var stop ir.StopReason
	for _, ev := range dec.Finish() {
		if ev.Type == ir.EvMessageDelta {
			stop = ev.StopReason
		}
	}
	if stop != ir.StopContentFilter {
		t.Errorf("stop_reason = %q, want content_filter", stop)
	}
}

func aggregateStream(t *testing.T, raw string) *ir.Response {
	t.Helper()
	dec := newStreamDecoder()
	var agg ir.Aggregator
	scanner := codec.NewFrameScanner(strings.NewReader(raw))
	for scanner.Scan() {
		frame := scanner.Frame()
		events, err := dec.Feed(frame.Event, frame.Data)
		if err != nil {
			t.Fatalf("feed %q: %v", frame.Data, err)
		}
		for _, ev := range events {
			agg.Add(ev)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, ev := range dec.Finish() {
		agg.Add(ev)
	}
	return agg.Response()
}
