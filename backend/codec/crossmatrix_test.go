package codec_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	_ "github.com/aceaura/model-surge-agent/backend/codec/anthropic"
	_ "github.com/aceaura/model-surge-agent/backend/codec/chatcompletions"
	_ "github.com/aceaura/model-surge-agent/backend/codec/responses"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 这个文件是三入站 × 三出站的交叉矩阵。测的不是「编码结果长什么样」——
// 那属于各协议自己的测试——而是四类语义能否穿过 IR 抵达对面：
// 工具调用、推理、用量、终止原因。矩阵覆盖是必要的：同协议往返能过
// 不代表跨协议能过，反之亦然。
//
// gemini 出站在阶段七加入，届时矩阵变成三 × 四。

// inboundNames 与 outboundNames 从注册表取，而非写死列表：
// 新增协议忘了加进矩阵时，这里会自动带上。
func inboundNames() []string  { return codec.InboundNames() }
func outboundNames() []string { return codec.OutboundNames() }

// requestFixture 是一份四类语义齐全的请求，按各入站协议给出等价写法。
// 三份 body 必须解出语义等价的 IR，这是矩阵成立的前提。
var requestFixture = map[string]string{
	codec.ProtocolAnthropic: `{
	  "model": "user-model",
	  "max_tokens": 2048,
	  "temperature": 0.7,
	  "system": "be terse",
	  "thinking": {"type": "enabled", "budget_tokens": 8192},
	  "tools": [{
	    "name": "grep",
	    "description": "search files",
	    "input_schema": {"type":"object","properties":{"pattern":{"type":"string"}}}
	  }],
	  "tool_choice": {"type": "auto"},
	  "messages": [
	    {"role": "user", "content": "find TODO"},
	    {"role": "assistant", "content": [
	      {"type": "thinking", "thinking": "need to grep", "signature": "sig-a"},
	      {"type": "tool_use", "id": "call_1", "name": "grep", "input": {"pattern": "TODO"}}
	    ]},
	    {"role": "user", "content": [
	      {"type": "tool_result", "tool_use_id": "call_1", "content": "found 3"}
	    ]}
	  ]
	}`,

	codec.ProtocolChatCompletions: `{
	  "model": "user-model",
	  "max_tokens": 2048,
	  "temperature": 0.7,
	  "reasoning_effort": "medium",
	  "tools": [{"type":"function","function":{
	    "name": "grep",
	    "description": "search files",
	    "parameters": {"type":"object","properties":{"pattern":{"type":"string"}}}
	  }}],
	  "tool_choice": "auto",
	  "messages": [
	    {"role": "system", "content": "be terse"},
	    {"role": "user", "content": "find TODO"},
	    {"role": "assistant", "reasoning_content": "need to grep", "tool_calls": [
	      {"index":0,"id":"call_1","type":"function","function":{"name":"grep","arguments":"{\"pattern\":\"TODO\"}"}}
	    ]},
	    {"role": "tool", "tool_call_id": "call_1", "content": "found 3"}
	  ]
	}`,

	codec.ProtocolResponses: `{
	  "model": "user-model",
	  "max_output_tokens": 2048,
	  "temperature": 0.7,
	  "instructions": "be terse",
	  "reasoning": {"effort": "medium"},
	  "tools": [{
	    "type": "function",
	    "name": "grep",
	    "description": "search files",
	    "parameters": {"type":"object","properties":{"pattern":{"type":"string"}}}
	  }],
	  "tool_choice": "auto",
	  "input": [
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"find TODO"}]},
	    {"type":"reasoning","summary":[{"type":"summary_text","text":"need to grep"}]},
	    {"type":"function_call","call_id":"call_1","name":"grep","arguments":"{\"pattern\":\"TODO\"}"},
	    {"type":"function_call_output","call_id":"call_1","output":"found 3"}
	  ]
	}`,
}

// TestInboundRequestsAgreeOnSemantics 证明三份等价 body 解出等价 IR。
// 后续所有出站断言都建立在这个前提上：若入站已经不等价，
// 出站的差异就无法归因。
func TestInboundRequestsAgreeOnSemantics(t *testing.T) {
	for _, name := range inboundNames() {
		body, ok := requestFixture[name]
		if !ok {
			t.Fatalf("inbound %q has no request fixture: add one or the matrix skips it", name)
		}
		c, _ := codec.Inbound(name)
		req, err := c.DecodeRequest([]byte(body))
		if err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}

		if got := systemText(req.System); got != "be terse" {
			t.Errorf("%s: system = %q, want %q", name, got, "be terse")
		}
		if req.MaxTokens != 2048 {
			t.Errorf("%s: max_tokens = %d, want 2048", name, req.MaxTokens)
		}
		if req.Temperature == nil || *req.Temperature != 0.7 {
			t.Errorf("%s: temperature = %v, want 0.7", name, req.Temperature)
		}
		if len(req.Tools) != 1 || req.Tools[0].Name != "grep" {
			t.Fatalf("%s: tools = %+v, want one named grep", name, req.Tools)
		}
		if req.ToolChoice == nil || req.ToolChoice.Mode != ir.ToolChoiceAuto {
			t.Errorf("%s: tool_choice = %+v, want auto", name, req.ToolChoice)
		}
		if req.Thinking == nil || !req.Thinking.Enabled {
			t.Errorf("%s: thinking not enabled", name)
		}

		use := findToolUse(req)
		if use == nil {
			t.Fatalf("%s: no tool_use block survived decoding", name)
		}
		if use.ID != "call_1" || use.Name != "grep" {
			t.Errorf("%s: tool_use = %+v, want call_1/grep", name, use)
		}
		if !strings.Contains(use.Input, "TODO") {
			t.Errorf("%s: tool_use input = %q, want it to carry the pattern", name, use.Input)
		}

		result := findToolResult(req)
		if result == nil {
			t.Fatalf("%s: no tool_result block survived decoding", name)
		}
		if result.ToolUseID != "call_1" {
			t.Errorf("%s: tool_result id = %q, want call_1", name, result.ToolUseID)
		}
		if got := systemText(result.Content); got != "found 3" {
			t.Errorf("%s: tool_result content = %q, want %q", name, got, "found 3")
		}

		think := findThinking(req)
		if think == nil || think.Text != "need to grep" {
			t.Errorf("%s: thinking = %+v, want text %q", name, think, "need to grep")
		}
	}
}

// TestRequestMatrixPreservesToolAndThinking 是请求侧的九格矩阵：
// 每格断言工具定义、工具调用、工具结果与思考请求都抵达了出站请求体。
func TestRequestMatrixPreservesToolAndThinking(t *testing.T) {
	for _, in := range inboundNames() {
		for _, out := range outboundNames() {
			t.Run(in+"→"+out, func(t *testing.T) {
				body := encodeAcross(t, in, out, requestFixture[in])
				text := string(body)

				// 工具定义与调用的名字在四协议里字段位置不同，但名字本身必须出现。
				for _, want := range []string{"grep", "call_1", "TODO", "found 3"} {
					if !strings.Contains(text, want) {
						t.Errorf("%q missing from wire body: %s", want, text)
					}
				}
				// 思考请求必须以目标协议的方式表达出来，不能被静默丢掉。
				if !hasThinkingRequest(t, out, body) {
					t.Errorf("thinking request did not survive: %s", text)
				}
				// 上游一律流式，否则数据面的单一解码路径不成立。
				if !isStreaming(t, body) {
					t.Errorf("outbound body must request streaming: %s", text)
				}
			})
		}
	}
}

// TestRequestMatrixCarriesNoForeignSignature 断言签名不跨族透传。
// 各协议的签名（Anthropic 的 signature、Responses 的 encrypted_content）
// 只有本族能验，发给别家会被拒。
func TestRequestMatrixCarriesNoForeignSignature(t *testing.T) {
	body := encodeAcross(t, codec.ProtocolAnthropic, codec.ProtocolChatCompletions,
		requestFixture[codec.ProtocolAnthropic])
	if strings.Contains(string(body), "sig-a") {
		t.Errorf("anthropic signature leaked into a chat_completions body: %s", body)
	}
	body = encodeAcross(t, codec.ProtocolAnthropic, codec.ProtocolResponses,
		requestFixture[codec.ProtocolAnthropic])
	if strings.Contains(string(body), "sig-a") {
		t.Errorf("anthropic signature leaked into a responses body: %s", body)
	}
	// 同族则必须保留：丢掉签名会让 Anthropic 拒绝带思考的多轮请求。
	body = encodeAcross(t, codec.ProtocolAnthropic, codec.ProtocolAnthropic,
		requestFixture[codec.ProtocolAnthropic])
	if !strings.Contains(string(body), "sig-a") {
		t.Errorf("anthropic signature must survive within the same family: %s", body)
	}
}

// streamFixture 是各出站协议的一段流，四类语义齐全：
// 文本、推理、工具调用（分片入参）、用量、终止原因。
var streamFixture = map[string]string{
	codec.ProtocolAnthropic: strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"native","role":"assistant","usage":{"input_tokens":120,"cache_read_input_tokens":30}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"pondering"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"searching"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call_9","name":"grep","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"pattern\":"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"TODO\"}"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":2}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":45}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n"),

	codec.ProtocolChatCompletions: strings.Join([]string{
		`data: {"id":"msg_1","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		``,
		`data: {"id":"msg_1","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{"reasoning_content":"pondering"}}]}`,
		``,
		`data: {"id":"msg_1","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{"content":"searching"}}]}`,
		``,
		`data: {"id":"msg_1","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"grep","arguments":"{\"pattern\":"}}]}}]}`,
		``,
		`data: {"id":"msg_1","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"TODO\"}"}}]}}]}`,
		``,
		`data: {"id":"msg_1","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: {"id":"msg_1","object":"chat.completion.chunk","model":"native","choices":[],"usage":{"prompt_tokens":120,"completion_tokens":45,"prompt_tokens_details":{"cached_tokens":30}}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"),

	codec.ProtocolResponses: strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"msg_1","model":"native","status":"in_progress"}}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning"}}`,
		``,
		`event: response.reasoning_summary_text.delta`,
		`data: {"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"pondering"}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning"}}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"message","role":"assistant"}}`,
		``,
		`event: response.content_part.added`,
		`data: {"type":"response.content_part.added","output_index":1,"content_index":0,"part":{"type":"output_text"}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","output_index":1,"content_index":0,"delta":"searching"}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"message","role":"assistant"}}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","call_id":"call_9","name":"grep"}}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","output_index":2,"delta":"{\"pattern\":"}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","output_index":2,"delta":"\"TODO\"}"}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","call_id":"call_9","name":"grep","arguments":"{\"pattern\":\"TODO\"}"}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"msg_1","model":"native","status":"completed","output":[{"type":"function_call","call_id":"call_9","name":"grep"}],"usage":{"input_tokens":120,"output_tokens":45,"input_tokens_details":{"cached_tokens":30}}}}`,
		``,
	}, "\n"),
}

// TestUpstreamStreamsAgreeOnSemantics 证明三份等价上游流解出等价的聚合结果。
// 与请求侧同理：这是流式九格矩阵的前提。
func TestUpstreamStreamsAgreeOnSemantics(t *testing.T) {
	for _, name := range outboundNames() {
		raw, ok := streamFixture[name]
		if !ok {
			t.Fatalf("outbound %q has no stream fixture: add one or the matrix skips it", name)
		}
		resp := aggregate(t, name, raw)

		if resp.Model != "native" {
			t.Errorf("%s: model = %q, want native", name, resp.Model)
		}
		if resp.StopReason != ir.StopToolUse {
			t.Errorf("%s: stop_reason = %q, want tool_use", name, resp.StopReason)
		}
		want := ir.Usage{InputTokens: 120, OutputTokens: 45, CacheReadTokens: 30}
		if resp.Usage != want {
			t.Errorf("%s: usage = %+v, want %+v", name, resp.Usage, want)
		}

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
				if b.Thinking != nil {
					thinking += b.Thinking.Text
				}
			case ir.BlockToolUse:
				use = b.ToolUse
			}
		}
		if text != "searching" {
			t.Errorf("%s: text = %q, want %q", name, text, "searching")
		}
		if thinking != "pondering" {
			t.Errorf("%s: thinking = %q, want %q", name, thinking, "pondering")
		}
		if use == nil {
			t.Fatalf("%s: no tool_use block in the aggregate", name)
		}
		if use.ID != "call_9" || use.Name != "grep" {
			t.Errorf("%s: tool_use = %+v, want call_9/grep", name, use)
		}
		// 入参必须拼成合法 JSON：分片边界处理错了这里就会失败。
		if !json.Valid([]byte(use.Input)) {
			t.Errorf("%s: tool input is not valid JSON: %q", name, use.Input)
		}
		var args struct{ Pattern string }
		if err := json.Unmarshal([]byte(use.Input), &args); err != nil || args.Pattern != "TODO" {
			t.Errorf("%s: tool input = %q, want pattern TODO", name, use.Input)
		}
	}
}

// TestStreamMatrixPreservesFourSemantics 是流式九格矩阵。
// 每格把上游流解成 IR，再编成客户端协议，然后用该协议自己的出站解码器
// 读回来——这是唯一能同时验证编码格式与语义保真的方式：若编出的帧
// 格式不对，解码就会失败或丢内容。
func TestStreamMatrixPreservesFourSemantics(t *testing.T) {
	for _, up := range outboundNames() {
		for _, client := range inboundNames() {
			t.Run(up+"→"+client, func(t *testing.T) {
				events := decodeStream(t, up, streamFixture[up])
				rendered := renderStream(t, client, events)
				resp := aggregate(t, client, rendered)

				if resp.StopReason != ir.StopToolUse {
					t.Errorf("stop_reason = %q, want tool_use\n%s", resp.StopReason, rendered)
				}
				want := ir.Usage{InputTokens: 120, OutputTokens: 45, CacheReadTokens: 30}
				if resp.Usage != want {
					t.Errorf("usage = %+v, want %+v\n%s", resp.Usage, want, rendered)
				}

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
						if b.Thinking != nil {
							thinking += b.Thinking.Text
						}
					case ir.BlockToolUse:
						use = b.ToolUse
					}
				}
				if text != "searching" {
					t.Errorf("text = %q, want %q\n%s", text, "searching", rendered)
				}
				if thinking != "pondering" {
					t.Errorf("thinking = %q, want %q\n%s", thinking, "pondering", rendered)
				}
				if use == nil {
					t.Fatalf("tool_use lost in translation\n%s", rendered)
				}
				if use.ID != "call_9" || use.Name != "grep" {
					t.Errorf("tool_use = %+v, want call_9/grep\n%s", use, rendered)
				}
				var args struct{ Pattern string }
				if err := json.Unmarshal([]byte(use.Input), &args); err != nil || args.Pattern != "TODO" {
					t.Errorf("tool input = %q, want pattern TODO\n%s", use.Input, rendered)
				}
			})
		}
	}
}

// TestStreamMatrixEmitsWellFormedTermination 断言每格都以该协议的
// 终止形态收尾。缺终止帧的客户端会一直等，症状是「界面卡住不出字」。
func TestStreamMatrixEmitsWellFormedTermination(t *testing.T) {
	for _, up := range outboundNames() {
		for _, client := range inboundNames() {
			t.Run(up+"→"+client, func(t *testing.T) {
				events := decodeStream(t, up, streamFixture[up])
				rendered := renderStream(t, client, events)
				if want := terminator(client); !strings.Contains(rendered, want) {
					t.Errorf("stream must end with %q:\n%s", want, rendered)
				}
			})
		}
	}
}

// terminator 是各协议的流终止标志。
func terminator(protocol string) string {
	switch protocol {
	case codec.ProtocolAnthropic:
		return "message_stop"
	case codec.ProtocolChatCompletions:
		return "[DONE]"
	case codec.ProtocolResponses:
		return "response.completed"
	default:
		return ""
	}
}

// TestErrorMatrixKeepsContextExceededOutOfRetries 断言上下文超限在每个
// 出站协议上都被识别为不可重试。归错类会让调度层把它记成目标失败并冷却，
// 而换目标对超长输入没有帮助。
func TestErrorMatrixKeepsContextExceededOutOfRetries(t *testing.T) {
	bodies := map[string]string{
		codec.ProtocolAnthropic:       `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 250000 tokens > 200000"}}`,
		codec.ProtocolChatCompletions: `{"error":{"message":"This model's maximum context length is 128000 tokens","type":"invalid_request_error","code":"context_length_exceeded"}}`,
		codec.ProtocolResponses:       `{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Input length exceeds the maximum context window"}}`,
	}
	for _, name := range outboundNames() {
		body, ok := bodies[name]
		if !ok {
			t.Fatalf("outbound %q has no context-overflow fixture", name)
		}
		c, _ := codec.Outbound(name)
		err := c.DecodeError(400, []byte(body))
		if err.Kind != ir.ErrContextExceeded {
			t.Errorf("%s: kind = %q, want context_exceeded", name, err.Kind)
		}
		if err.Retryable {
			t.Errorf("%s: context overflow must not be retryable", name)
		}
	}
}

// TestErrorMatrixMapsRateLimitToRetryable 断言 429 在每个协议上都可重试：
// 换个目标确实可能成功，这是调度层存在的意义。
func TestErrorMatrixMapsRateLimitToRetryable(t *testing.T) {
	for _, name := range outboundNames() {
		c, _ := codec.Outbound(name)
		err := c.DecodeError(429, []byte(`{"error":{"message":"slow down"}}`))
		if err.Kind != ir.ErrRateLimit {
			t.Errorf("%s: kind = %q, want rate_limit", name, err.Kind)
		}
		if !err.Retryable {
			t.Errorf("%s: rate limit must be retryable", name)
		}
	}
}

// TestInboundRenderErrorUsesProtocolShape 断言错误响应用的是客户端协议
// 自己的形状：SDK 按自己的格式解析，形状不对会被当成解码失败而非业务错误。
func TestInboundRenderErrorUsesProtocolShape(t *testing.T) {
	marker := map[string]string{
		codec.ProtocolAnthropic:       `"type":"error"`,
		codec.ProtocolChatCompletions: `"type":"invalid_request_error"`,
		codec.ProtocolResponses:       `"type":"invalid_request_error"`,
	}
	for _, name := range inboundNames() {
		c, _ := codec.Inbound(name)
		status, body := c.RenderError(ir.NewError(ir.ErrInvalidRequest, 0, "", "bad field"))
		if status != 400 {
			t.Errorf("%s: status = %d, want 400", name, status)
		}
		if !strings.Contains(string(body), marker[name]) {
			t.Errorf("%s: body = %s, want it to contain %s", name, body, marker[name])
		}
		if !strings.Contains(string(body), "bad field") {
			t.Errorf("%s: body must carry the message: %s", name, body)
		}
	}
}

// TestNoProtocolDropsMaxTokensStopReason 断言 max_tokens 截断在九格里都能传达。
// 客户端靠它决定要不要续写，丢了会让长回答看起来像是自然结束。
func TestNoProtocolDropsMaxTokensStopReason(t *testing.T) {
	for _, client := range inboundNames() {
		t.Run(client, func(t *testing.T) {
			events := []ir.Event{
				{Type: ir.EvMessageStart, MessageID: "msg_1", Model: "native"},
				{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
				{Type: ir.EvTextDelta, Index: 0, Text: "truncated"},
				{Type: ir.EvBlockStop, Index: 0},
				{Type: ir.EvMessageDelta, StopReason: ir.StopMaxTokens,
					Usage: &ir.Usage{InputTokens: 10, OutputTokens: 2048}},
				{Type: ir.EvMessageStop},
			}
			rendered := renderStream(t, client, events)
			resp := aggregate(t, client, rendered)
			if resp.StopReason != ir.StopMaxTokens {
				t.Errorf("stop_reason = %q, want max_tokens\n%s", resp.StopReason, rendered)
			}
		})
	}
}

// --- helpers ---

// encodeAcross 走一遍完整的入站解码 → 出站编码。
func encodeAcross(t *testing.T, in, out, body string) []byte {
	t.Helper()
	ic, ok := codec.Inbound(in)
	if !ok {
		t.Fatalf("inbound %q not registered", in)
	}
	req, err := ic.DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("%s decode: %v", in, err)
	}
	oc, ok := codec.Outbound(out)
	if !ok {
		t.Fatalf("outbound %q not registered", out)
	}
	// native model 由 pipeline 在编码前替换，这里模拟那一步，
	// 以免断言把用户模型名当成上游模型名。
	req.Model = "native"
	encoded, err := oc.EncodeRequest(req)
	if err != nil {
		t.Fatalf("%s encode: %v", out, err)
	}
	return encoded
}

// decodeStream 把一段 SSE 喂给出站解码器，收集全部 IR 事件。
func decodeStream(t *testing.T, protocol, raw string) []ir.Event {
	t.Helper()
	c, ok := codec.Outbound(protocol)
	if !ok {
		t.Fatalf("outbound %q not registered", protocol)
	}
	dec := c.NewStreamDecoder()
	var out []ir.Event
	scanner := codec.NewFrameScanner(strings.NewReader(raw))
	for scanner.Scan() {
		frame := scanner.Frame()
		events, err := dec.Feed(frame.Event, frame.Data)
		if err != nil {
			t.Fatalf("%s feed %q: %v", protocol, frame.Data, err)
		}
		out = append(out, events...)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("%s scan: %v", protocol, err)
	}
	return append(out, dec.Finish()...)
}

// renderStream 把 IR 事件编成客户端协议的 SSE 文本。
func renderStream(t *testing.T, protocol string, events []ir.Event) string {
	t.Helper()
	c, ok := codec.Inbound(protocol)
	if !ok {
		t.Fatalf("inbound %q not registered", protocol)
	}
	enc := c.NewStreamEncoder()
	var b strings.Builder
	for _, ev := range events {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("%s encode %s: %v", protocol, ev.Type, err)
		}
		for _, f := range frames {
			b.Write(f)
		}
	}
	for _, f := range enc.Finish() {
		b.Write(f)
	}
	return b.String()
}

// aggregate 用该协议的出站解码器读一段流，再聚合成 Response。
// 客户端协议同时也是某个上游的协议，所以可以拿它自己的解码器验自己的编码。
func aggregate(t *testing.T, protocol, raw string) *ir.Response {
	t.Helper()
	var agg ir.Aggregator
	for _, ev := range decodeStream(t, protocol, raw) {
		agg.Add(ev)
	}
	return agg.Response()
}

// hasThinkingRequest 按各协议的表达方式检查思考请求是否抵达 wire body。
func hasThinkingRequest(t *testing.T, protocol string, body []byte) bool {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("%s: unmarshal body: %v", protocol, err)
	}
	switch protocol {
	case codec.ProtocolAnthropic:
		return len(m["thinking"]) > 0
	case codec.ProtocolChatCompletions:
		return len(m["reasoning_effort"]) > 0
	case codec.ProtocolResponses:
		return len(m["reasoning"]) > 0
	default:
		return false
	}
}

func isStreaming(t *testing.T, body []byte) bool {
	t.Helper()
	var m struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	return m.Stream
}

func systemText(blocks []ir.Block) string {
	var b strings.Builder
	for _, block := range blocks {
		b.WriteString(block.Text)
	}
	return b.String()
}

func findToolUse(req *ir.Request) *ir.ToolUse {
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockToolUse {
				return b.ToolUse
			}
		}
	}
	return nil
}

func findToolResult(req *ir.Request) *ir.ToolResult {
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockToolResult {
				return b.ToolResult
			}
		}
	}
	return nil
}

func findThinking(req *ir.Request) *ir.Thinking {
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockThinking {
				return b.Thinking
			}
		}
	}
	return nil
}
