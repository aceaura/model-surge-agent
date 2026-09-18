package codec_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	_ "github.com/aceaura/model-surge-agent/backend/codec/anthropic"
	_ "github.com/aceaura/model-surge-agent/backend/codec/chatcompletions"
	_ "github.com/aceaura/model-surge-agent/backend/codec/gemini"
	_ "github.com/aceaura/model-surge-agent/backend/codec/responses"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 这个文件是三入站 × 四出站的交叉矩阵。测的不是「编码结果长什么样」——
// 那属于各协议自己的测试——而是四类语义能否穿过 IR 抵达对面：
// 工具调用、推理、用量、终止原因。矩阵覆盖是必要的：同协议往返能过
// 不代表跨协议能过，反之亦然。
//
// gemini 只出现在出站一侧：本服务不对客户端暴露它的接口。所以请求侧的
// 入站 fixture 只有三份，而出站格数是四。

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
		if !req.Thinking.On() {
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
				if !requestsStreaming(t, out, body) {
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

// TestOutboundLossyEncodingIsByteStable 钉住有损诊断的零副作用性质。
//
// 两条路径必须产出同一份字节：请求体只要因为「是否收集诊断」而漂移，
// 提示缓存前缀就跟着变，命中率会无声地掉下去。
func TestOutboundLossyEncodingIsByteStable(t *testing.T) {
	for _, in := range inboundNames() {
		for _, out := range outboundNames() {
			t.Run(in+"->"+out, func(t *testing.T) {
				req := decodeRequest(t, in, requestFixture[in])
				oc, ok := codec.Outbound(out)
				if !ok {
					t.Fatalf("outbound %q not registered", out)
				}
				le, ok := oc.(codec.LossyEncoder)
				if !ok {
					t.Fatalf("outbound %q must implement LossyEncoder", out)
				}
				plain, err := oc.EncodeRequest(req.Clone())
				if err != nil {
					t.Fatalf("EncodeRequest: %v", err)
				}
				lossyBody, _, err := le.EncodeRequestLossy(req.Clone())
				if err != nil {
					t.Fatalf("EncodeRequestLossy: %v", err)
				}
				if string(plain) != string(lossyBody) {
					t.Errorf("bodies drifted between the two paths:\n%s\n%s", plain, lossyBody)
				}
			})
		}
	}
}

// TestRequestMatrixReportsCrossFamilySignatureAsLossy 要求剥离与诊断一致：
// 编码器悄悄丢了签名而诊断不报，排查的人就无从知道请求被改过。
func TestRequestMatrixReportsCrossFamilySignatureAsLossy(t *testing.T) {
	for _, out := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolGemini} {
		notes := lossyAcross(t, codec.ProtocolAnthropic, out)
		if !strings.Contains(strings.Join(notes, "\n"), "thinking signature") {
			t.Errorf("%s dropped the anthropic signature without reporting it: %v", out, notes)
		}
	}
	notes := lossyAcross(t, codec.ProtocolAnthropic, codec.ProtocolAnthropic)
	if strings.Contains(strings.Join(notes, "\n"), "thinking signature") {
		t.Errorf("same-family signature must not be reported as dropped: %v", notes)
	}
}

// decodeRequest 解出 IR 并替换 native model，与 encodeAcross 的前半段同构。
func decodeRequest(t *testing.T, in, body string) *ir.Request {
	t.Helper()
	ic, ok := codec.Inbound(in)
	if !ok {
		t.Fatalf("inbound %q not registered", in)
	}
	req, err := ic.DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("%s decode: %v", in, err)
	}
	req.Model = "native"
	return req
}

func lossyAcross(t *testing.T, in, out string) []string {
	t.Helper()
	req := decodeRequest(t, in, requestFixture[in])
	oc, ok := codec.Outbound(out)
	if !ok {
		t.Fatalf("outbound %q not registered", out)
	}
	le, ok := oc.(codec.LossyEncoder)
	if !ok {
		t.Fatalf("outbound %q must implement LossyEncoder", out)
	}
	_, notes, err := le.EncodeRequestLossy(req)
	if err != nil {
		t.Fatalf("EncodeRequestLossy: %v", err)
	}
	return notes
}

// streamFixture 是各出站协议的一段流，四类语义齐全：
// 文本、推理、工具调用（分片入参）、用量、终止原因。
//
// 四份都描述同一次调用：输入总量 120（其中缓存命中 30）、输出 45。
// anthropic 的 input_tokens 不含缓存命中故写 90，另外三家的对应字段
// 含缓存故写 120 —— 这个差异正是 IR 要抹平的口径漂移。
var streamFixture = map[string]string{
	codec.ProtocolAnthropic: strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"native","role":"assistant","usage":{"input_tokens":90,"cache_read_input_tokens":30}}}`,
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

	// gemini 这份与另外三份的形态差别最大：帧无 event 名，每帧都是一个完整
	// 响应对象，函数调用的 args 一次到齐不分片，输出用量拆成
	// candidatesTokenCount + thoughtsTokenCount，末尾报 STOP 而非工具调用，
	// 且没有终止帧——终止事件由解码器的 Finish 补出。
	codec.ProtocolGemini: strings.Join([]string{
		`data: {"responseId":"msg_1","modelVersion":"native","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"pondering","thought":true}]}}],"usageMetadata":{"promptTokenCount":120,"cachedContentTokenCount":30}}`,
		``,
		`data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"search"},{"text":"ing"}]}}]}`,
		``,
		`data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"id":"call_9","name":"grep","args":{"pattern":"TODO"}}}]}}],"usageMetadata":{"promptTokenCount":120,"candidatesTokenCount":40,"thoughtsTokenCount":5,"cachedContentTokenCount":30}}`,
		``,
		`data: {"candidates":[{"index":0,"finishReason":"STOP"}]}`,
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
		// IR 口径：InputTokens 是不含缓存的新鲜输入，总量仍为 90+30=120。
		// 推理维度不在此断言：只有部分上游的 fixture 带这个字段。
		want := ir.Usage{InputTokens: 90, OutputTokens: 45, CacheReadTokens: 30}
		got := resp.Usage
		got.ReasoningTokens = 0
		if got != want {
			t.Errorf("%s: usage = %+v, want %+v", name, got, want)
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
				// IR 口径：InputTokens 不含缓存命中。推理维度另有专门测试，
				// 因为它能否穿过取决于客户端协议表达得了没有。
				want := ir.Usage{InputTokens: 90, OutputTokens: 45, CacheReadTokens: 30}
				got := resp.Usage
				got.ReasoningTokens = 0
				if got != want {
					t.Errorf("usage = %+v, want %+v\n%s", got, want, rendered)
				}
				// 守恒：无论走哪一格，客户端可见的输入总量都得是 120。
				if total := resp.Usage.InputTokens + resp.Usage.CacheReadTokens; total != 120 {
					t.Errorf("total input = %d, want 120\n%s", total, rendered)
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
		codec.ProtocolGemini:          `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"The input token count exceeds the maximum context window of 1048576"}}`,
	}
	for _, name := range outboundNames() {
		body, ok := bodies[name]
		if !ok {
			t.Fatalf("outbound %q has no context-overflow fixture", name)
		}
		c, _ := codec.Outbound(name)
		err := c.DecodeError(400, nil, []byte(body))
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
		err := c.DecodeError(429, nil, []byte(`{"error":{"message":"slow down"}}`))
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
	case codec.ProtocolGemini:
		var cfg struct {
			ThinkingConfig json.RawMessage `json:"thinkingConfig"`
		}
		if err := json.Unmarshal(m["generationConfig"], &cfg); err != nil {
			return false
		}
		return len(cfg.ThinkingConfig) > 0
	default:
		return false
	}
}

// requestsStreaming 检查该格确实向上游要了流式。
//
// 三个协议靠请求体的 stream 字段表达，gemini 没有这个字段——它靠
// Endpoint 的方法名与 alt=sse，所以这一格只能验端点。
func requestsStreaming(t *testing.T, protocol string, body []byte) bool {
	t.Helper()
	if protocol == codec.ProtocolGemini {
		c, _ := codec.Outbound(protocol)
		url, _ := c.Endpoint("https://host/v1beta", "native", true)
		return strings.Contains(url, ":streamGenerateContent") && strings.Contains(url, "alt=sse")
	}
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

// --- 畸形请求矩阵 ---

// malformedFixtures 是四类真实会被上游 400 拒收的畸形请求，每类都以三种
// 入站协议的等价写法给出。矩阵要证明的不是「修得漂亮」，而是修完之后
// 四个出站协议都拿得到自己能接受的请求体——修复发生在 IR 层，
// 但被拒收的是 wire body，只有编码后才能验。
//
// verify 只断言结构（某个 wire 标记在或不在、两段内容的先后），不断言
// 原始入参字节：四个出站编码器都会把非法工具入参改写成 {}，
// 断言字节会把这层防御误判成缺陷。
var malformedFixtures = []struct {
	name   string
	bodies map[string]string
	verify func(t *testing.T, outProtocol string, encoded []byte)
}{
	{
		// 孤儿工具结果：结果找不到宣告它的 tool_use。Sanitize 会把它降级为
		// 文本，所以出站体里内容还在，但不能再以工具结果的形态出现——
		// 没有宣告方的工具结果是上游拒收的直接原因。
		name: "orphan-tool-result",
		bodies: map[string]string{
			codec.ProtocolAnthropic: `{
			  "model": "user-model",
			  "max_tokens": 1024,
			  "messages": [
			    {"role": "user", "content": "hi"},
			    {"role": "user", "content": [
			      {"type": "tool_result", "tool_use_id": "call_x", "content": "orphan output"}
			    ]}
			  ]
			}`,
			codec.ProtocolChatCompletions: `{
			  "model": "user-model",
			  "max_tokens": 1024,
			  "messages": [
			    {"role": "user", "content": "hi"},
			    {"role": "tool", "tool_call_id": "call_x", "content": "orphan output"}
			  ]
			}`,
			codec.ProtocolResponses: `{
			  "model": "user-model",
			  "max_output_tokens": 1024,
			  "input": [
			    {"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			    {"type":"function_call_output","call_id":"call_x","output":"orphan output"}
			  ]
			}`,
		},
		verify: func(t *testing.T, out string, encoded []byte) {
			text := string(encoded)
			if !strings.Contains(text, "orphan output") {
				t.Errorf("降级后内容丢失: %s", text)
			}
			if marker := toolResultMarker(out); strings.Contains(text, marker) {
				t.Errorf("孤儿结果仍以工具结果形态发出（%s）: %s", marker, text)
			}
		},
	},
	{
		// 错序结果：两个并行调用的结果按相反顺序给出。上游按顺序把结果
		// 绑回调用，错序会让参数与结果对错，所以出站体里 a 必须排在 b 前。
		name: "misordered-tool-results",
		bodies: map[string]string{
			codec.ProtocolAnthropic: `{
			  "model": "user-model",
			  "max_tokens": 1024,
			  "messages": [
			    {"role": "user", "content": "find"},
			    {"role": "assistant", "content": [
			      {"type": "tool_use", "id": "call_a", "name": "grep", "input": {"pattern": "A"}},
			      {"type": "tool_use", "id": "call_b", "name": "ls", "input": {"path": "/"}}
			    ]},
			    {"role": "user", "content": [
			      {"type": "tool_result", "tool_use_id": "call_b", "content": "b out"},
			      {"type": "tool_result", "tool_use_id": "call_a", "content": "a out"}
			    ]}
			  ]
			}`,
			codec.ProtocolChatCompletions: `{
			  "model": "user-model",
			  "max_tokens": 1024,
			  "messages": [
			    {"role": "user", "content": "find"},
			    {"role": "assistant", "tool_calls": [
			      {"index":0,"id":"call_a","type":"function","function":{"name":"grep","arguments":"{\"pattern\":\"A\"}"}},
			      {"index":1,"id":"call_b","type":"function","function":{"name":"ls","arguments":"{\"path\":\"/\"}"}}
			    ]},
			    {"role": "tool", "tool_call_id": "call_b", "content": "b out"},
			    {"role": "tool", "tool_call_id": "call_a", "content": "a out"}
			  ]
			}`,
			codec.ProtocolResponses: `{
			  "model": "user-model",
			  "max_output_tokens": 1024,
			  "input": [
			    {"type":"message","role":"user","content":[{"type":"input_text","text":"find"}]},
			    {"type":"function_call","call_id":"call_a","name":"grep","arguments":"{\"pattern\":\"A\"}"},
			    {"type":"function_call","call_id":"call_b","name":"ls","arguments":"{\"path\":\"/\"}"},
			    {"type":"function_call_output","call_id":"call_b","output":"b out"},
			    {"type":"function_call_output","call_id":"call_a","output":"a out"}
			  ]
			}`,
		},
		verify: func(t *testing.T, out string, encoded []byte) {
			text := string(encoded)
			ai := strings.Index(text, "a out")
			bi := strings.Index(text, "b out")
			if ai < 0 || bi < 0 {
				t.Fatalf("结果内容缺失 a=%d b=%d: %s", ai, bi, text)
			}
			if ai > bi {
				t.Errorf("结果顺序未与调用对齐（a 应在 b 前）: %s", text)
			}
		},
	},
	{
		// 悬空调用：助手宣告了工具却没有任何结果，上下文压缩截断后的典型形态。
		// Sanitize 丢弃该调用，同一条消息里的正文必须留下。
		name: "unanswered-tool-use",
		bodies: map[string]string{
			codec.ProtocolAnthropic: `{
			  "model": "user-model",
			  "max_tokens": 1024,
			  "messages": [
			    {"role": "user", "content": "hi"},
			    {"role": "assistant", "content": [
			      {"type": "text", "text": "let me look"},
			      {"type": "tool_use", "id": "call_z", "name": "ls", "input": {"path": "/"}}
			    ]}
			  ]
			}`,
			codec.ProtocolChatCompletions: `{
			  "model": "user-model",
			  "max_tokens": 1024,
			  "messages": [
			    {"role": "user", "content": "hi"},
			    {"role": "assistant", "content": "let me look", "tool_calls": [
			      {"index":0,"id":"call_z","type":"function","function":{"name":"ls","arguments":"{\"path\":\"/\"}"}}
			    ]}
			  ]
			}`,
			codec.ProtocolResponses: `{
			  "model": "user-model",
			  "max_output_tokens": 1024,
			  "input": [
			    {"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			    {"type":"message","role":"assistant","content":[{"type":"output_text","text":"let me look"}]},
			    {"type":"function_call","call_id":"call_z","name":"ls","arguments":"{\"path\":\"/\"}"}
			  ]
			}`,
		},
		verify: func(t *testing.T, out string, encoded []byte) {
			text := string(encoded)
			if strings.Contains(text, "call_z") {
				t.Errorf("悬空调用仍被发往上游: %s", text)
			}
			if !strings.Contains(text, "let me look") {
				t.Errorf("同消息内的正文被连带丢弃: %s", text)
			}
		},
	},
	{
		// 空白 prefill：尾部助手消息只有空白。Anthropic 对此直接 400
		// （text content blocks must contain non-whitespace text）。
		// 用纯空白而非零块消息：responses 协议表达不出零块消息，
		// 空白文本是三家都能等价写出的形态。
		name: "whitespace-prefill",
		bodies: map[string]string{
			codec.ProtocolAnthropic: `{
			  "model": "user-model",
			  "max_tokens": 1024,
			  "messages": [
			    {"role": "user", "content": "hi"},
			    {"role": "assistant", "content": "   "}
			  ]
			}`,
			codec.ProtocolChatCompletions: `{
			  "model": "user-model",
			  "max_tokens": 1024,
			  "messages": [
			    {"role": "user", "content": "hi"},
			    {"role": "assistant", "content": "   "}
			  ]
			}`,
			codec.ProtocolResponses: `{
			  "model": "user-model",
			  "max_output_tokens": 1024,
			  "input": [
			    {"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			    {"type":"message","role":"assistant","content":[{"type":"output_text","text":"   "}]}
			  ]
			}`,
		},
		verify: func(t *testing.T, out string, encoded []byte) {
			text := string(encoded)
			if !strings.Contains(text, "hi") {
				t.Errorf("正常消息被误删: %s", text)
			}
			if marker := assistantRoleMarker(out); strings.Contains(text, marker) {
				t.Errorf("空白 prefill 仍被发出（%s）: %s", marker, text)
			}
		},
	},
}

// TestMalformedFixturesAgreeOnSemantics 是矩阵的前置断言：三份等价 body
// 必须解出语义等价的 IR。比的是扁平化的块序列而非消息布局——
// responses 会把同角色的相邻条目并进一条消息，消息条数天然不同，
// 但块的种类与顺序必须一致，否则出站差异无法归因。
func TestMalformedFixturesAgreeOnSemantics(t *testing.T) {
	for _, fx := range malformedFixtures {
		t.Run(fx.name, func(t *testing.T) {
			var want string
			for _, in := range inboundNames() {
				body, ok := fx.bodies[in]
				if !ok {
					t.Fatalf("inbound %q 缺 %q fixture: 补一份，否则这一列被静默跳过", in, fx.name)
				}
				c, _ := codec.Inbound(in)
				req, err := c.DecodeRequest([]byte(body))
				if err != nil {
					t.Fatalf("%s decode: %v", in, err)
				}
				got := semanticSnapshot(req)
				if want == "" {
					want = got
					continue
				}
				if got != want {
					t.Errorf("%s 解出的语义与其他入站不等价\n got: %s\nwant: %s", in, got, want)
				}
			}
		})
	}
}

// TestMalformedRequestMatrixProducesAcceptableBodies 是四类畸形 × 三入站 ×
// 四出站的十二格。走的是与 pipeline 相同的顺序：入站解码 → Sanitize →
// 出站编码，因为畸形是在 IR 层修的，而被上游拒收的是编码后的 wire body。
func TestMalformedRequestMatrixProducesAcceptableBodies(t *testing.T) {
	for _, fx := range malformedFixtures {
		for _, in := range inboundNames() {
			for _, out := range outboundNames() {
				t.Run(fx.name+"/"+in+"→"+out, func(t *testing.T) {
					encoded := sanitizeAcross(t, in, out, fx.bodies[in])
					if !json.Valid(encoded) {
						t.Fatalf("编出的请求体不是合法 JSON: %s", encoded)
					}
					fx.verify(t, out, encoded)
				})
			}
		}
	}
}

// sanitizeAcross 与 encodeAcross 的区别只在中间多一步 Sanitize，
// 对齐 pipeline 的真实顺序。
func sanitizeAcross(t *testing.T, in, out, body string) []byte {
	t.Helper()
	ic, ok := codec.Inbound(in)
	if !ok {
		t.Fatalf("inbound %q not registered", in)
	}
	req, err := ic.DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("%s decode: %v", in, err)
	}
	ir.Sanitize(req)
	req.Model = "native"
	oc, ok := codec.Outbound(out)
	if !ok {
		t.Fatalf("outbound %q not registered", out)
	}
	encoded, err := oc.EncodeRequest(req)
	if err != nil {
		t.Fatalf("%s encode: %v", out, err)
	}
	return encoded
}

// semanticSnapshot 把请求压成「块种类 + 工具 id + 文本」的扁平序列，
// 跨过消息边界：消息布局属于各协议的表达自由，块序列才是语义。
func semanticSnapshot(req *ir.Request) string {
	var sb strings.Builder
	for _, m := range req.Messages {
		for _, b := range m.Content {
			sb.WriteString(string(b.Type))
			switch {
			case b.ToolUse != nil:
				sb.WriteString(":" + b.ToolUse.ID + "/" + b.ToolUse.Name)
			case b.ToolResult != nil:
				sb.WriteString(":" + b.ToolResult.ToolUseID + "/" + systemText(b.ToolResult.Content))
			default:
				sb.WriteString(":" + b.Text)
			}
			sb.WriteString("|")
		}
	}
	return sb.String()
}

// toolResultMarker 是各出站协议表达「这是一个工具结果」的 wire 标记。
func toolResultMarker(protocol string) string {
	switch protocol {
	case codec.ProtocolAnthropic:
		return `"type":"tool_result"`
	case codec.ProtocolChatCompletions:
		return `"role":"tool"`
	case codec.ProtocolResponses:
		return `"function_call_output"`
	case codec.ProtocolGemini:
		return `"functionResponse"`
	default:
		return ""
	}
}

// assistantRoleMarker 是各出站协议表达助手消息的 wire 标记。
func assistantRoleMarker(protocol string) string {
	if protocol == codec.ProtocolGemini {
		return `"role":"model"`
	}
	return `"role":"assistant"`
}

// --- 畸形上游流矩阵 ---

// malformedStreams 是三类真实出现过的畸形上游流。frames 只给能表达该形态的
// 出站协议：有些形态在某些协议里根本写不出来（如 gemini 的函数调用 args
// 一次到齐，表达不出截断），漏掉的格子不是覆盖缺口而是协议事实。
var malformedStreams = []struct {
	name   string
	frames map[string]string
	verify func(t *testing.T, client string, agg *ir.Aggregator, rendered string)
}{
	{
		// 首帧全量：上游在块开启帧就给出完整入参，之后不再发增量。
		// 解码器若把开启帧的入参当占位清掉，整个调用的参数就静默丢了。
		name: "tool-args-complete-on-first-frame",
		frames: map[string]string{
			codec.ProtocolAnthropic: strings.Join([]string{
				`event: message_start`,
				`data: {"type":"message_start","message":{"id":"msg_1","model":"native","role":"assistant","usage":{"input_tokens":10}}}`,
				``,
				`event: content_block_start`,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_9","name":"grep","input":{"pattern":"TODO"}}}`,
				``,
				`event: content_block_stop`,
				`data: {"type":"content_block_stop","index":0}`,
				``,
				`event: message_delta`,
				`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`,
				``,
				`event: message_stop`,
				`data: {"type":"message_stop"}`,
				``,
			}, "\n"),
			codec.ProtocolChatCompletions: strings.Join([]string{
				`data: {"id":"msg_1","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"grep","arguments":"{\"pattern\":\"TODO\"}"}}]}}]}`,
				``,
				`data: {"id":"msg_1","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				``,
				`data: [DONE]`,
				``,
			}, "\n"),
			codec.ProtocolResponses: strings.Join([]string{
				`event: response.created`,
				`data: {"type":"response.created","response":{"id":"msg_1","model":"native","status":"in_progress"}}`,
				``,
				`event: response.output_item.added`,
				`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_9","name":"grep","arguments":"{\"pattern\":\"TODO\"}"}}`,
				``,
				`event: response.output_item.done`,
				`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"call_9","name":"grep","arguments":"{\"pattern\":\"TODO\"}"}}`,
				``,
				`event: response.completed`,
				`data: {"type":"response.completed","response":{"id":"msg_1","model":"native","status":"completed","output":[{"type":"function_call","call_id":"call_9","name":"grep"}],"usage":{"input_tokens":10,"output_tokens":5}}}`,
				``,
			}, "\n"),
			// gemini 的函数调用本来就只有这一种形态。
			codec.ProtocolGemini: strings.Join([]string{
				`data: {"responseId":"msg_1","modelVersion":"native","candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"id":"call_9","name":"grep","args":{"pattern":"TODO"}}}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5}}`,
				``,
				`data: {"candidates":[{"index":0,"finishReason":"STOP"}]}`,
				``,
			}, "\n"),
		},
		verify: func(t *testing.T, client string, agg *ir.Aggregator, rendered string) {
			assertCompleteGrepCall(t, agg, rendered)
		},
	},
	{
		// 先 args 后 name：首个分片只有 arguments，id 与 name 在后续分片才到。
		// 只有 chat_completions 能表达——另外三家的块开启帧必带 name。
		// 解码器必须缓冲这些分片，边到边发会产出一个无名调用。
		name: "tool-args-before-name",
		frames: map[string]string{
			codec.ProtocolChatCompletions: strings.Join([]string{
				`data: {"id":"msg_1","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
				``,
				`data: {"id":"msg_1","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"pattern\":"}}]}}]}`,
				``,
				`data: {"id":"msg_1","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"grep","arguments":"\"TODO\"}"}}]}}]}`,
				``,
				`data: {"id":"msg_1","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				``,
				`data: [DONE]`,
				``,
			}, "\n"),
		},
		verify: func(t *testing.T, client string, agg *ir.Aggregator, rendered string) {
			assertCompleteGrepCall(t, agg, rendered)
		},
	},
	{
		// 截断入参：上游在 arguments 发到一半断流。这种响应不能当成功，
		// 客户端会把残缺调用存进历史，下一轮重放时整个请求都会被上游拒收。
		// 矩阵这里只验「畸形能穿过转换被识别出来」——处置在 pipeline 层。
		name: "tool-args-truncated",
		frames: map[string]string{
			codec.ProtocolAnthropic: strings.Join([]string{
				`event: message_start`,
				`data: {"type":"message_start","message":{"id":"msg_1","model":"native","role":"assistant","usage":{"input_tokens":10}}}`,
				``,
				`event: content_block_start`,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_9","name":"grep","input":{}}}`,
				``,
				`event: content_block_delta`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"pattern\":"}}`,
				``,
				`event: message_delta`,
				`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`,
				``,
				`event: message_stop`,
				`data: {"type":"message_stop"}`,
				``,
			}, "\n"),
			codec.ProtocolChatCompletions: strings.Join([]string{
				`data: {"id":"msg_1","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"grep","arguments":"{\"pattern\":"}}]}}]}`,
				``,
				`data: {"id":"msg_1","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				``,
				`data: [DONE]`,
				``,
			}, "\n"),
			codec.ProtocolResponses: strings.Join([]string{
				`event: response.created`,
				`data: {"type":"response.created","response":{"id":"msg_1","model":"native","status":"in_progress"}}`,
				``,
				`event: response.output_item.added`,
				`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_9","name":"grep"}}`,
				``,
				`event: response.function_call_arguments.delta`,
				`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"pattern\":"}`,
				``,
				`event: response.incomplete`,
				`data: {"type":"response.incomplete","response":{"id":"msg_1","model":"native","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":10,"output_tokens":5}}}`,
				``,
			}, "\n"),
			// gemini 缺席：它的 args 一次到齐，表达不出截断。
		},
		verify: func(t *testing.T, client string, agg *ir.Aggregator, rendered string) {
			bad := agg.IncompleteTools()
			if len(bad) == 0 {
				t.Errorf("截断入参未被识别，客户端会把残缺调用存进历史\n%s", rendered)
			}
		},
	},
}

// TestMalformedStreamMatrixSurvivesTranslation 把每份畸形流按「出站解码 →
// 入站编码 → 用该协议自己的解码器读回」走一遍，断言畸形形态在转换后
// 仍能被正确识别或修复。用协议自己的解码器读回是唯一能同时验证
// 编码格式与语义的方式。
func TestMalformedStreamMatrixSurvivesTranslation(t *testing.T) {
	for _, fx := range malformedStreams {
		for _, up := range outboundNames() {
			raw, ok := fx.frames[up]
			if !ok {
				continue
			}
			for _, client := range inboundNames() {
				t.Run(fx.name+"/"+up+"→"+client, func(t *testing.T) {
					events := decodeStream(t, up, raw)
					rendered := renderStream(t, client, events)
					fx.verify(t, client, aggregator(t, client, rendered), rendered)
				})
			}
		}
	}
}

// assertCompleteGrepCall 断言聚合结果里有一个入参完整可解析的 grep 调用。
func assertCompleteGrepCall(t *testing.T, agg *ir.Aggregator, rendered string) {
	t.Helper()
	if bad := agg.IncompleteTools(); len(bad) > 0 {
		t.Errorf("入参被判为截断 %v，但上游给的是完整入参\n%s", bad, rendered)
	}
	var use *ir.ToolUse
	for _, b := range agg.Response().Content {
		if b.Type == ir.BlockToolUse {
			use = b.ToolUse
		}
	}
	if use == nil {
		t.Fatalf("tool_use 在转换中丢失\n%s", rendered)
	}
	if use.Name != "grep" {
		t.Errorf("tool name = %q, want grep\n%s", use.Name, rendered)
	}
	var args struct{ Pattern string }
	if err := json.Unmarshal([]byte(use.Input), &args); err != nil || args.Pattern != "TODO" {
		t.Errorf("tool input = %q, want pattern TODO\n%s", use.Input, rendered)
	}
}

// aggregator 与 aggregate 的区别是返回聚合器本身，
// 以便断言 IncompleteTools 这类只在聚合器上暴露的判定。
func aggregator(t *testing.T, protocol, raw string) *ir.Aggregator {
	t.Helper()
	agg := &ir.Aggregator{}
	for _, ev := range decodeStream(t, protocol, raw) {
		agg.Add(ev)
	}
	return agg
}

// --- 用量守恒矩阵 ---

// cacheUsageFixture 是同一次调用在四个出站协议里的用量写法：
// 输入总量 100，其中缓存命中 30，输出 20。
//
// anthropic 的 input_tokens 不含缓存故写 70，另外三家的对应字段含缓存
// 故写 100 —— IR 统一按「不含缓存」存，编回客户端时再加上。
var cacheUsageFixture = map[string]string{
	codec.ProtocolAnthropic: strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_u","model":"native","role":"assistant","usage":{"input_tokens":70,"cache_read_input_tokens":30}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":20}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n"),

	codec.ProtocolChatCompletions: strings.Join([]string{
		`data: {"id":"msg_u","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"}}]}`,
		``,
		`data: {"id":"msg_u","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		``,
		`data: {"id":"msg_u","object":"chat.completion.chunk","model":"native","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30}}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"),

	codec.ProtocolResponses: strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"msg_u","model":"native","status":"in_progress"}}`,
		``,
		`event: response.content_part.added`,
		`data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text"}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"ok"}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant"}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"msg_u","model":"native","status":"completed","usage":{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"cached_tokens":30}}}}`,
		``,
	}, "\n"),

	codec.ProtocolGemini: strings.Join([]string{
		`data: {"responseId":"msg_u","modelVersion":"native","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":20,"cachedContentTokenCount":30}}`,
		``,
		`data: {"candidates":[{"index":0,"finishReason":"STOP"}]}`,
		``,
	}, "\n"),
}

// TestUsageConservationAcrossMatrix 走遍十二格，断言客户端可见的输入总量
// 恒为 100。守恒是这里唯一有意义的口径：各协议对「input 含不含缓存」
// 的定义不同，只有总量在换算前后必须一致；漂移会直接体现为账单差异。
func TestUsageConservationAcrossMatrix(t *testing.T) {
	for _, up := range outboundNames() {
		for _, client := range inboundNames() {
			t.Run(up+"→"+client, func(t *testing.T) {
				events := decodeStream(t, up, cacheUsageFixture[up])
				rendered := renderStream(t, client, events)
				resp := aggregate(t, client, rendered)

				want := ir.Usage{InputTokens: 70, OutputTokens: 20, CacheReadTokens: 30}
				if resp.Usage != want {
					t.Errorf("usage = %+v, want %+v\n%s", resp.Usage, want, rendered)
				}
				if total := resp.Usage.InputTokens + resp.Usage.CacheReadTokens; total != 100 {
					t.Errorf("输入总量 = %d, want 100\n%s", total, rendered)
				}
			})
		}
	}
}

// reasoningUsageFixture 是带推理用量的一段流。三家的写法不同：
// chat_completions 与 responses 的输出总量已含推理（20 含 8），
// gemini 的 thoughtsTokenCount 不含在 candidatesTokenCount 里（12+8=20）。
// 三份都描述同一次调用：输出 20，其中推理 8。
//
// anthropic 不在此表：它没有推理用量字段，作为上游给不出这一维。
var reasoningUsageFixture = map[string]string{
	codec.ProtocolChatCompletions: strings.Join([]string{
		`data: {"id":"msg_u","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`,
		``,
		`data: {"id":"msg_u","object":"chat.completion.chunk","model":"native","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"completion_tokens_details":{"reasoning_tokens":8}}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"),

	codec.ProtocolResponses: strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"msg_u","model":"native","status":"in_progress"}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"ok"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"msg_u","model":"native","status":"completed","usage":{"input_tokens":100,"output_tokens":20,"output_tokens_details":{"reasoning_tokens":8}}}}`,
		``,
	}, "\n"),

	codec.ProtocolGemini: strings.Join([]string{
		`data: {"responseId":"msg_u","modelVersion":"native","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":12,"thoughtsTokenCount":8}}`,
		``,
		`data: {"candidates":[{"index":0,"finishReason":"STOP"}]}`,
		``,
	}, "\n"),
}

// 推理维度的可表达性因客户端协议而异，所以断言分两档：能表达的要求
// 数字原样穿过；表达不了的只要求它仍含在输出总量里——那不是信息丢失，
// 只是看不见推理占比，故也不报有损。
func TestReasoningTokensSurviveWhereExpressible(t *testing.T) {
	expressible := map[string]bool{
		codec.ProtocolChatCompletions: true,
		codec.ProtocolResponses:       true,
		codec.ProtocolAnthropic:       false,
	}
	for up, raw := range reasoningUsageFixture {
		for client, canExpress := range expressible {
			t.Run(up+"→"+client, func(t *testing.T) {
				events := decodeStream(t, up, raw)
				rendered := renderStream(t, client, events)
				resp := aggregate(t, client, rendered)

				if resp.Usage.OutputTokens != 20 {
					t.Errorf("output = %d, want 20 (reasoning is billed as output)\n%s",
						resp.Usage.OutputTokens, rendered)
				}
				if canExpress {
					if resp.Usage.ReasoningTokens != 8 {
						t.Errorf("reasoning = %d, want 8\n%s", resp.Usage.ReasoningTokens, rendered)
					}
					return
				}
				if resp.Usage.ReasoningTokens != 0 {
					t.Errorf("reasoning = %d, %s cannot express it\n%s",
						resp.Usage.ReasoningTokens, client, rendered)
				}
			})
		}
	}
}

// cacheWriteUsageFixture 是带缓存写入量的一段流：输入总量 100（缓存命中 30、
// 缓存写入 25、新鲜输入 70），输出 20。缓存写入按各协议口径都是独立计量的
// 一笔，不含在输入总量里，所以守恒断言只管 70+30。
//
// responses 与 gemini 不在此表：两家的用量结构里没有缓存写入字段，
// 作为上游给不出这一维。
var cacheWriteUsageFixture = map[string]string{
	codec.ProtocolAnthropic: strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_w","model":"native","role":"assistant","usage":{"input_tokens":70,"cache_read_input_tokens":30,"cache_creation_input_tokens":25}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":20}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n"),

	// 本协议没有官方的缓存写入字段，兼容层通行的别名是 cache_creation_tokens。
	codec.ProtocolChatCompletions: strings.Join([]string{
		`data: {"id":"msg_w","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`,
		``,
		`data: {"id":"msg_w","object":"chat.completion.chunk","model":"native","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30},"cache_creation_tokens":25}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"),
}

// TestCacheWriteTokensSurviveWhereExpressible 与推理维度同构地分两档：
// 客户端表达得了就要求数字原样穿过，表达不了就要求它归零而不是被折进
// 别的口径——缓存写入单价与新鲜输入不同，混进 input 会直接算错账。
// 两档都要求输入输出总量守恒。
func TestCacheWriteTokensSurviveWhereExpressible(t *testing.T) {
	expressible := map[string]bool{
		codec.ProtocolAnthropic:       true,
		codec.ProtocolChatCompletions: true,
		codec.ProtocolResponses:       false,
	}
	for _, up := range outboundNames() {
		raw, ok := cacheWriteUsageFixture[up]
		if !ok {
			continue
		}
		for _, client := range inboundNames() {
			canExpress, known := expressible[client]
			if !known {
				t.Fatalf("入站 %q 未登记 cache_write 可表达性", client)
			}
			t.Run(up+"→"+client, func(t *testing.T) {
				rendered := renderStream(t, client, decodeStream(t, up, raw))
				resp := aggregate(t, client, rendered)

				if resp.Usage.InputTokens != 70 || resp.Usage.CacheReadTokens != 30 {
					t.Errorf("input/cache_read = %d/%d, want 70/30\n%s",
						resp.Usage.InputTokens, resp.Usage.CacheReadTokens, rendered)
				}
				if resp.Usage.OutputTokens != 20 {
					t.Errorf("output = %d, want 20\n%s", resp.Usage.OutputTokens, rendered)
				}
				if canExpress {
					if resp.Usage.CacheWriteTokens != 25 {
						t.Errorf("cache_write = %d, want 25\n%s", resp.Usage.CacheWriteTokens, rendered)
					}
					return
				}
				if resp.Usage.CacheWriteTokens != 0 {
					t.Errorf("cache_write = %d，%s 表达不了这一维\n%s",
						resp.Usage.CacheWriteTokens, client, rendered)
				}
			})
		}
	}
}

// --- 有损说明矩阵 ---

// lossyProbe 是一个只带单一敏感特征的请求。矩阵拿它逐字段扫过四个出站协议：
// 能力位为真必须一条说明都不出，为假必须恰有一条——多出一条意味着诊断把
// 同一次丢弃报了两遍，少一条意味着字段被无声丢掉。
type lossyProbe struct {
	// field 是说明文本里该字段的标签，与 DescribeLossy 的 note 首参一致。
	field string
	build func() *ir.Request
	// expressible 回答该出站协议表达得了这个特征没有。
	expressible func(codec.Capabilities) bool
}

// probeRequest 的 max_tokens 取 8192 而非一个小值：anthropic 要求推理预算
// 同时不低于 1024 且小于 max_tokens，max_tokens 太小时两个约束无解，
// shapeParams 会（正确地）关掉 thinking，thinking 探针就测不到能力位了。
func probeRequest(blocks ...ir.Block) *ir.Request {
	return &ir.Request{
		Model:     "native",
		MaxTokens: 8192,
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
			{Role: ir.RoleAssistant, Content: blocks},
		},
	}
}

func mediaProbe(kind ir.BlockType, mediaType, data, name string) lossyProbe {
	return lossyProbe{
		field: string(kind) + " blocks",
		build: func() *ir.Request {
			return probeRequest(ir.Block{
				Type:  kind,
				Media: &ir.Media{MediaType: mediaType, Data: data, Name: name},
			})
		},
		expressible: func(c codec.Capabilities) bool { return c.AcceptsMedia(mediaType) },
	}
}

func lossyProbes() []lossyProbe {
	return []lossyProbe{
		{
			field: "tools",
			build: func() *ir.Request {
				req := probeRequest(ir.Block{Type: ir.BlockText, Text: "ok"})
				req.Tools = []ir.Tool{{Name: "grep", Description: "search",
					Schema: `{"type":"object","properties":{"pattern":{"type":"string"}}}`}}
				return req
			},
			expressible: func(c codec.Capabilities) bool { return c.Tools },
		},
		{
			field: "top_k",
			build: func() *ir.Request {
				req := probeRequest(ir.Block{Type: ir.BlockText, Text: "ok"})
				k := 40
				req.TopK = &k
				return req
			},
			expressible: func(c codec.Capabilities) bool { return c.TopK },
		},
		{
			field: "stop_sequences",
			build: func() *ir.Request {
				req := probeRequest(ir.Block{Type: ir.BlockText, Text: "ok"})
				req.StopSequences = []string{"\n\n"}
				return req
			},
			expressible: func(c codec.Capabilities) bool { return c.StopSequences },
		},
		{
			field: "thinking",
			build: func() *ir.Request {
				req := probeRequest(ir.Block{Type: ir.BlockText, Text: "ok"})
				req.Thinking = &ir.ThinkingConfig{Enabled: ir.ThinkingOn(), Effort: "medium", BudgetTokens: 4096}
				return req
			},
			expressible: func(c codec.Capabilities) bool { return c.Thinking },
		},
		{
			field: "cache_control",
			build: func() *ir.Request {
				return probeRequest(ir.Block{Type: ir.BlockText, Text: "ok", CacheCtl: "ephemeral"})
			},
			expressible: func(c codec.Capabilities) bool { return c.CacheControl },
		},
		{
			field: "thinking blocks",
			build: func() *ir.Request {
				return probeRequest(ir.Block{Type: ir.BlockThinking,
					Thinking: &ir.Thinking{Text: "pondering"}})
			},
			expressible: func(c codec.Capabilities) bool { return c.Thinking },
		},
		mediaProbe(ir.BlockImage, "image/png", pngPixel, "shot.png"),
		mediaProbe(ir.BlockAudio, "audio/wav", wavClip, "clip.wav"),
		mediaProbe(ir.BlockDocument, "application/pdf", pdfDoc, "spec.pdf"),
		mediaProbe(ir.BlockFile, "text/plain", textFile, "notes.txt"),
	}
}

// TestLossyNoteMatrixTracksCapabilities 是有损说明矩阵。
//
// 断言的是「能力位与说明一一对应」而非某个协议的固定清单：后者一改 Caps
// 就得同步改测试，前者会自动跟着能力位走，新出站协议进注册表即入矩阵。
func TestLossyNoteMatrixTracksCapabilities(t *testing.T) {
	for _, out := range outboundNames() {
		oc, ok := codec.Outbound(out)
		if !ok {
			t.Fatalf("outbound %q not registered", out)
		}
		caps := oc.Caps()
		for _, probe := range lossyProbes() {
			t.Run(out+"/"+probe.field, func(t *testing.T) {
				body, notes := lossyOf(t, out, probe.build())
				if !json.Valid(body) {
					t.Fatalf("encoded body is not valid JSON: %s", body)
				}
				if probe.expressible(caps) {
					if len(notes) != 0 {
						t.Errorf("%s 表达得了 %s，不该有说明：%v", out, probe.field, notes)
					}
					return
				}
				if len(notes) != 1 {
					t.Fatalf("%s 表达不了 %s，应恰有一条说明，实得 %v", out, probe.field, notes)
				}
				if !strings.Contains(notes[0], probe.field) {
					t.Errorf("说明未点名字段 %s：%s", probe.field, notes[0])
				}
			})
		}
	}
}

// TestSignatureLossIsFamilyScoped 单独验签名维度：它有两个前置条件
// （能力位与同族来源），塞进上面的字段矩阵会把两件事混成一条断言。
func TestSignatureLossIsFamilyScoped(t *testing.T) {
	for _, out := range outboundNames() {
		oc, _ := codec.Outbound(out)
		caps := oc.Caps()
		if !caps.Thinking {
			continue
		}

		t.Run(out+"/same-family", func(t *testing.T) {
			req := probeRequest(ir.Block{Type: ir.BlockThinking,
				Thinking: &ir.Thinking{Text: "pondering", Signature: "sig-abc", SignatureFrom: out}})
			_, notes := lossyOf(t, out, req)
			if caps.ThinkingSig {
				if len(notes) != 0 {
					t.Errorf("%s 支持签名且来源同族，不该有说明：%v", out, notes)
				}
				return
			}
			if len(notes) != 1 || !strings.Contains(notes[0], "thinking signature") {
				t.Errorf("%s 不支持签名，应恰有一条签名说明，实得 %v", out, notes)
			}
		})

		t.Run(out+"/foreign-family", func(t *testing.T) {
			// 别家来源的签名一律剥离，与本协议支持签名与否无关。
			req := probeRequest(ir.Block{Type: ir.BlockThinking,
				Thinking: &ir.Thinking{Text: "pondering", Signature: "sig-abc",
					SignatureFrom: "some-other-protocol"}})
			_, notes := lossyOf(t, out, req)
			if len(notes) != 1 || !strings.Contains(notes[0], "thinking signature") {
				t.Errorf("%s 应剥离别家签名并恰报一条，实得 %v", out, notes)
			}
		})
	}
}

// TestRedactedThinkingIsAlwaysLossy 断言加密推理块在每个出站协议上都报有损，
// 包括同族的 anthropic。载荷不可解读，谁都重编不出来——这是唯一与能力位
// 无关的丢弃，所以不放进按能力位断言的字段矩阵。
func TestRedactedThinkingIsAlwaysLossy(t *testing.T) {
	for _, out := range outboundNames() {
		t.Run(out, func(t *testing.T) {
			req := probeRequest(ir.Block{Type: ir.BlockThinking,
				Thinking: &ir.Thinking{Redacted: true, SignatureFrom: out}})
			_, notes := lossyOf(t, out, req)
			if len(notes) != 1 || !strings.Contains(notes[0], "redacted_thinking") {
				t.Errorf("%s 应恰报一条 redacted_thinking，实得 %v", out, notes)
			}
		})
	}
}

// lossyOf 走出站有损编码，并顺带校验两条路径逐字节一致：
// EncodeRequest 与 EncodeRequestLossy 若漂移，缓存前缀会跟着漂。
func lossyOf(t *testing.T, out string, req *ir.Request) ([]byte, []string) {
	t.Helper()
	oc, ok := codec.Outbound(out)
	if !ok {
		t.Fatalf("outbound %q not registered", out)
	}
	le, ok := oc.(codec.LossyEncoder)
	if !ok {
		t.Fatalf("outbound %q must implement LossyEncoder", out)
	}
	body, notes, err := le.EncodeRequestLossy(req.Clone())
	if err != nil {
		t.Fatalf("%s EncodeRequestLossy: %v", out, err)
	}
	plain, err := oc.EncodeRequest(req.Clone())
	if err != nil {
		t.Fatalf("%s EncodeRequest: %v", out, err)
	}
	if !bytes.Equal(body, plain) {
		t.Fatalf("%s: 两条编码路径产出不同请求体\n lossy: %s\n plain: %s", out, body, plain)
	}
	return body, notes
}

// --- stop_reason 矩阵 ---

// allStopReasons 是 IR 的全部终止原因。写死一份而非从代码反射：
// 新增取值时这里不会自动带上，编译也不报错，所以下面另有一处计数断言兜底。
var allStopReasons = []ir.StopReason{
	ir.StopEndTurn,
	ir.StopMaxTokens,
	ir.StopStopSequence,
	ir.StopToolUse,
	ir.StopContentFilter,
}

// upstreamStopWire 给出各上游协议表达某个 IR 终止原因所用的线上取值。
//
// 取值缺失表示该协议压根没有对应的线上表达，不是本服务丢了它：
// stop_sequence 只有 anthropic 有专门取值，另外三家一律并入「正常结束」。
// 这类格不进矩阵也不用 t.Skip——写不出 fixture 的格没有可断言的对象，
// 放宽断言反而会把「协议本就如此」记成通过。
var upstreamStopWire = map[string]map[ir.StopReason]string{
	codec.ProtocolAnthropic: {
		ir.StopEndTurn:       "end_turn",
		ir.StopMaxTokens:     "max_tokens",
		ir.StopStopSequence:  "stop_sequence",
		ir.StopToolUse:       "tool_use",
		ir.StopContentFilter: "refusal",
	},
	codec.ProtocolChatCompletions: {
		ir.StopEndTurn:       "stop",
		ir.StopMaxTokens:     "length",
		ir.StopToolUse:       "tool_calls",
		ir.StopContentFilter: "content_filter",
	},
	codec.ProtocolResponses: {
		ir.StopEndTurn:       "completed",
		ir.StopMaxTokens:     "max_output_tokens",
		ir.StopToolUse:       "completed",
		ir.StopContentFilter: "content_filter",
	},
	codec.ProtocolGemini: {
		ir.StopEndTurn:   "STOP",
		ir.StopMaxTokens: "MAX_TOKENS",
		// 本协议以工具调用收尾时也报 STOP，tool_use 靠帧里有没有
		// functionCall 判定，不靠这个取值。
		ir.StopToolUse:       "STOP",
		ir.StopContentFilter: "SAFETY",
	},
}

// clientStopClass 给出终止原因经某客户端协议往返后应落到的取值。
//
// 有的协议会把多个 IR 取值折成同一个线上取值，往返回来就还原不出原值。
// 这不是缺陷，是协议能力差：断言等价类而非原值，才能既不放宽又不误判。
func clientStopClass(client string, reason ir.StopReason) ir.StopReason {
	switch client {
	case codec.ProtocolChatCompletions:
		// finish_reason 没有 stop_sequence，与正常结束同为 "stop"。
		if reason == ir.StopStopSequence {
			return ir.StopEndTurn
		}
	case codec.ProtocolResponses:
		// status 只有 completed / incomplete 两档，stop_sequence 归 completed。
		if reason == ir.StopStopSequence {
			return ir.StopEndTurn
		}
	}
	return reason
}

// stopReasonStream 造一段以指定原因收尾的上游流。tool_use 那一格额外
// 带上工具调用：三个协议里有两个靠帧内是否存在调用来判定 tool_use，
// 只改终止取值造不出这一格。
func stopReasonStream(t *testing.T, protocol string, reason ir.StopReason) string {
	t.Helper()
	wire, ok := upstreamStopWire[protocol][reason]
	if !ok {
		t.Fatalf("%s has no wire value for %s", protocol, reason)
	}
	withTool := reason == ir.StopToolUse

	switch protocol {
	case codec.ProtocolAnthropic:
		frames := []string{
			`event: message_start`,
			`data: {"type":"message_start","message":{"id":"msg_1","model":"native","usage":{"input_tokens":5}}}`,
			``,
			`event: content_block_start`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			``,
			`event: content_block_delta`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"body"}}`,
			``,
			`event: content_block_stop`,
			`data: {"type":"content_block_stop","index":0}`,
			``,
		}
		if withTool {
			frames = append(frames,
				`event: content_block_start`,
				`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_1","name":"grep","input":{}}}`,
				``,
				`event: content_block_delta`,
				`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"pattern\":\"TODO\"}"}}`,
				``,
				`event: content_block_stop`,
				`data: {"type":"content_block_stop","index":1}`,
				``,
			)
		}
		return strings.Join(append(frames,
			`event: message_delta`,
			`data: {"type":"message_delta","delta":{"stop_reason":"`+wire+`"},"usage":{"output_tokens":3}}`,
			``,
			`event: message_stop`,
			`data: {"type":"message_stop"}`,
			``,
		), "\n")

	case codec.ProtocolChatCompletions:
		frames := []string{
			`data: {"id":"msg_1","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{"role":"assistant","content":"body"}}]}`,
			``,
		}
		if withTool {
			frames = append(frames,
				`data: {"id":"msg_1","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"grep","arguments":"{\"pattern\":\"TODO\"}"}}]}}]}`,
				``,
			)
		}
		return strings.Join(append(frames,
			`data: {"id":"msg_1","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{},"finish_reason":"`+wire+`"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`,
			``,
			`data: [DONE]`,
			``,
		), "\n")

	case codec.ProtocolGemini:
		frames := []string{
			`data: {"responseId":"msg_1","modelVersion":"native","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"body"}]}}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3}}`,
			``,
		}
		if withTool {
			frames = append(frames,
				`data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"id":"call_1","name":"grep","args":{"pattern":"TODO"}}}]}}]}`,
				``,
			)
		}
		return strings.Join(append(frames,
			`data: {"candidates":[{"index":0,"finishReason":"`+wire+`"}]}`,
			``,
		), "\n")

	case codec.ProtocolResponses:
		frames := []string{
			`event: response.created`,
			`data: {"type":"response.created","response":{"id":"msg_1","model":"native","status":"in_progress"}}`,
			``,
			`event: response.output_item.added`,
			`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant"}}`,
			``,
			`event: response.content_part.added`,
			`data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text"}}`,
			``,
			`event: response.output_text.delta`,
			`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"body"}`,
			``,
			`event: response.output_item.done`,
			`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant"}}`,
			``,
		}
		output := ""
		if withTool {
			frames = append(frames,
				`event: response.output_item.added`,
				`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"grep"}}`,
				``,
				`event: response.function_call_arguments.delta`,
				`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"pattern\":\"TODO\"}"}`,
				``,
				`event: response.output_item.done`,
				`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"grep","arguments":"{\"pattern\":\"TODO\"}"}}`,
				``,
			)
			output = `,"output":[{"type":"function_call","call_id":"call_1","name":"grep"}]`
		}
		usage := `,"usage":{"input_tokens":5,"output_tokens":3}`
		// 截断与安全拦截都走 incomplete，靠 incomplete_details.reason 区分。
		if reason == ir.StopMaxTokens || reason == ir.StopContentFilter {
			return strings.Join(append(frames,
				`event: response.incomplete`,
				`data: {"type":"response.incomplete","response":{"id":"msg_1","model":"native","status":"incomplete","incomplete_details":{"reason":"`+wire+`"}`+output+usage+`}}`,
				``,
			), "\n")
		}
		return strings.Join(append(frames,
			`event: response.completed`,
			`data: {"type":"response.completed","response":{"id":"msg_1","model":"native","status":"`+wire+`"`+output+usage+`}}`,
			``,
		), "\n")

	default:
		t.Fatalf("no stop-reason stream builder for %q", protocol)
		return ""
	}
}

// TestStopReasonMatrixPreservesSemantics 是 stop_reason 矩阵：
// 每个 IR 取值 × 每个上游 × 每个客户端。终止原因错了，客户端会做错决定——
// 把截断当成自然结束就不去续写，把工具调用当成结束就不去执行工具。
func TestStopReasonMatrixPreservesSemantics(t *testing.T) {
	for _, reason := range allStopReasons {
		for _, up := range outboundNames() {
			if _, ok := upstreamStopWire[up][reason]; !ok {
				continue
			}
			for _, client := range inboundNames() {
				t.Run(string(reason)+"/"+up+"→"+client, func(t *testing.T) {
					events := decodeStream(t, up, stopReasonStream(t, up, reason))
					rendered := renderStream(t, client, events)
					resp := aggregate(t, client, rendered)

					if want := clientStopClass(client, reason); resp.StopReason != want {
						t.Errorf("stop_reason = %q, want %q\n%s", resp.StopReason, want, rendered)
					}
					if want := terminatorFor(client, reason); !strings.Contains(rendered, want) {
						t.Errorf("流未以 %q 收尾\n%s", want, rendered)
					}
				})
			}
		}
	}
}

// terminatorFor 在 terminator 之上按终止原因细化。
// responses 的终止帧名随结果而变：未跑完的流收在 response.incomplete，
// 断言恒定的 response.completed 会把正确行为记成缺陷。
func terminatorFor(client string, reason ir.StopReason) string {
	if client == codec.ProtocolResponses &&
		(reason == ir.StopMaxTokens || reason == ir.StopContentFilter) {
		return "response.incomplete"
	}
	return terminator(client)
}

// TestStopReasonWireTableCoversEveryUpstream 断言每个上游协议在表里都有
// 一份取值。漏一个协议时上面的矩阵会静默少跑一整片格子，这里把它变成失败。
func TestStopReasonWireTableCoversEveryUpstream(t *testing.T) {
	for _, up := range outboundNames() {
		table, ok := upstreamStopWire[up]
		if !ok {
			t.Fatalf("上游 %q 未登记 stop_reason 线上取值", up)
		}
		// 至少要能表达正常结束、截断、工具调用这三档，否则矩阵形同虚设。
		for _, must := range []ir.StopReason{ir.StopEndTurn, ir.StopMaxTokens, ir.StopToolUse} {
			if _, ok := table[must]; !ok {
				t.Errorf("上游 %q 缺 %s 的线上取值", up, must)
			}
		}
	}
}

// --- 媒体降级矩阵 ---

// mediaCase 是一类媒体在三种入站协议里的等价写法。
// wantKind 是解码后应落到的 IR 块类型，mediaType 用于查出站白名单。
type mediaCase struct {
	name      string
	wantKind  ir.BlockType
	mediaType string
	bodies    map[string]string
}

// mediaCases 覆盖四类媒体块。每类都给出三种入站写法：各协议的容器名与
// 载荷形态完全不同（anthropic 的 source 对象、chat_completions 的
// data URI、responses 的 input_file），但必须解出同一个 IR 块。
func mediaCases() []mediaCase {
	return []mediaCase{
		{
			name: "image", wantKind: ir.BlockImage, mediaType: "image/png",
			bodies: map[string]string{
				codec.ProtocolAnthropic: `{"model":"user-model","max_tokens":256,"messages":[
				  {"role":"user","content":[
				    {"type":"text","text":"look"},
				    {"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + pngPixel + `"}}
				  ]}
				]}`,
				codec.ProtocolChatCompletions: `{"model":"user-model","max_tokens":256,"messages":[
				  {"role":"user","content":[
				    {"type":"text","text":"look"},
				    {"type":"image_url","image_url":{"url":"data:image/png;base64,` + pngPixel + `"}}
				  ]}
				]}`,
				codec.ProtocolResponses: `{"model":"user-model","max_output_tokens":256,"input":[
				  {"role":"user","content":[
				    {"type":"input_text","text":"look"},
				    {"type":"input_image","image_url":"data:image/png;base64,` + pngPixel + `"}
				  ]}
				]}`,
			},
		},
		{
			name: "audio", wantKind: ir.BlockAudio, mediaType: "audio/wav",
			bodies: map[string]string{
				// anthropic 没有音频容器，只能塞进 document——解码按 media type
				// 判类型而非容器名，所以仍应落到 BlockAudio。
				codec.ProtocolAnthropic: `{"model":"user-model","max_tokens":256,"messages":[
				  {"role":"user","content":[
				    {"type":"text","text":"listen"},
				    {"type":"document","source":{"type":"base64","media_type":"audio/wav","data":"` + wavClip + `"}}
				  ]}
				]}`,
				codec.ProtocolChatCompletions: `{"model":"user-model","max_tokens":256,"messages":[
				  {"role":"user","content":[
				    {"type":"text","text":"listen"},
				    {"type":"input_audio","input_audio":{"format":"wav","data":"` + wavClip + `"}}
				  ]}
				]}`,
				codec.ProtocolResponses: `{"model":"user-model","max_output_tokens":256,"input":[
				  {"role":"user","content":[
				    {"type":"input_text","text":"listen"},
				    {"type":"input_audio","input_audio":{"format":"wav","data":"` + wavClip + `"}}
				  ]}
				]}`,
			},
		},
		{
			name: "document", wantKind: ir.BlockDocument, mediaType: "application/pdf",
			bodies: map[string]string{
				codec.ProtocolAnthropic: `{"model":"user-model","max_tokens":256,"messages":[
				  {"role":"user","content":[
				    {"type":"text","text":"read"},
				    {"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"` + pdfDoc + `"}}
				  ]}
				]}`,
				codec.ProtocolChatCompletions: `{"model":"user-model","max_tokens":256,"messages":[
				  {"role":"user","content":[
				    {"type":"text","text":"read"},
				    {"type":"file","file":{"filename":"spec.pdf","file_data":"data:application/pdf;base64,` + pdfDoc + `"}}
				  ]}
				]}`,
				codec.ProtocolResponses: `{"model":"user-model","max_output_tokens":256,"input":[
				  {"role":"user","content":[
				    {"type":"input_text","text":"read"},
				    {"type":"input_file","filename":"spec.pdf","file_data":"data:application/pdf;base64,` + pdfDoc + `"}
				  ]}
				]}`,
			},
		},
		{
			// 纯文本附件是「其余附件」的代表：多数协议只能降级成文本。
			name: "file", wantKind: ir.BlockFile, mediaType: "text/plain",
			bodies: map[string]string{
				codec.ProtocolAnthropic: `{"model":"user-model","max_tokens":256,"messages":[
				  {"role":"user","content":[
				    {"type":"text","text":"attached"},
				    {"type":"document","source":{"type":"base64","media_type":"text/plain","data":"` + textFile + `"}}
				  ]}
				]}`,
				codec.ProtocolChatCompletions: `{"model":"user-model","max_tokens":256,"messages":[
				  {"role":"user","content":[
				    {"type":"text","text":"attached"},
				    {"type":"file","file":{"filename":"notes.txt","file_data":"data:text/plain;base64,` + textFile + `"}}
				  ]}
				]}`,
				codec.ProtocolResponses: `{"model":"user-model","max_output_tokens":256,"input":[
				  {"role":"user","content":[
				    {"type":"input_text","text":"attached"},
				    {"type":"input_file","filename":"notes.txt","file_data":"data:text/plain;base64,` + textFile + `"}
				  ]}
				]}`,
			},
		},
	}
}

// TestMediaDecodesToSameKindAcrossInbound 是媒体矩阵的前提：三种入站写法
// 必须解出同一个 IR 块类型。这一步不过，下面的降级矩阵就无从比较。
func TestMediaDecodesToSameKindAcrossInbound(t *testing.T) {
	for _, mc := range mediaCases() {
		for _, in := range inboundNames() {
			t.Run(mc.name+"/"+in, func(t *testing.T) {
				body, ok := mc.bodies[in]
				if !ok {
					t.Fatalf("入站 %q 缺 %s 的 fixture", in, mc.name)
				}
				req := decodeRequest(t, in, body)
				block := findMedia(req)
				if block == nil {
					t.Fatalf("%s 未解出媒体块", in)
				}
				if block.Type != mc.wantKind {
					t.Errorf("块类型 = %q, want %q", block.Type, mc.wantKind)
				}
				if got := codec.SniffMediaType(block.Media); got != mc.mediaType {
					t.Errorf("media type = %q, want %q", got, mc.mediaType)
				}
			})
		}
	}
}

// TestMediaDowngradeMatrix 是媒体降级矩阵：四类媒体 × 3 入站 × 4 出站。
//
// 每格只有两种合法结局：出站白名单收得下就原生承载，收不下就改写成说明性
// 文本并报一条有损。二者都不成立意味着媒体被无声吞掉——模型看不到附件，
// 用户提到「上面那份文件」时它会答得莫名其妙，而日志里毫无线索。
func TestMediaDowngradeMatrix(t *testing.T) {
	for _, mc := range mediaCases() {
		for _, in := range inboundNames() {
			for _, out := range outboundNames() {
				t.Run(mc.name+"/"+in+"→"+out, func(t *testing.T) {
					req := decodeRequest(t, in, mc.bodies[in])
					body, notes := lossyOf(t, out, req)
					if !json.Valid(body) {
						t.Fatalf("请求体不是合法 JSON：%s", body)
					}

					oc, _ := codec.Outbound(out)
					native := oc.Caps().AcceptsMedia(mc.mediaType)
					// 音频还有一层：白名单收得下，但只接受内联 base64 加
					// 认得的格式名，两者缺一仍要降级。这里的 fixture 两者齐全。
					downgraded := strings.Contains(string(body), "attachment omitted")

					if native {
						if downgraded {
							t.Errorf("%s 支持 %s，不该降级：%s", out, mc.mediaType, body)
						}
						if len(notes) != 0 {
							t.Errorf("%s 支持 %s，不该报有损：%v", out, mc.mediaType, notes)
						}
						// 原生承载时载荷必须真的出现在请求体里。
						if !strings.Contains(string(body), mediaPayload(t, req)) {
							t.Errorf("%s 声称原生承载，但请求体里没有载荷：%s", out, body)
						}
						return
					}

					if !downgraded {
						t.Errorf("%s 表达不了 %s，应降级为说明文本：%s", out, mc.mediaType, body)
					}
					if len(notes) != 1 || !strings.Contains(notes[0], string(mc.wantKind)+" blocks") {
						t.Errorf("降级应恰报一条 %s blocks 有损，实得 %v", mc.wantKind, notes)
					}
					// 同轮的文本内容不能被降级顺手吃掉。
					if !strings.Contains(string(body), textOf(req)) {
						t.Errorf("降级丢了同消息的文本 %q：%s", textOf(req), body)
					}
				})
			}
		}
	}
}

// findMedia 取出请求里第一个媒体块。
func findMedia(req *ir.Request) *ir.Block {
	for i, m := range req.Messages {
		for j, b := range m.Content {
			if b.Type.IsMedia() {
				return &req.Messages[i].Content[j]
			}
		}
	}
	return nil
}

// mediaPayload 取媒体块的 base64 载荷，用于确认它确实进了请求体。
func mediaPayload(t *testing.T, req *ir.Request) string {
	t.Helper()
	b := findMedia(req)
	if b == nil || b.Media == nil || b.Media.Data == "" {
		t.Fatalf("fixture 应带内联 base64 载荷")
	}
	return b.Media.Data
}

// textOf 取请求里第一段文本。
func textOf(req *ir.Request) string {
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockText && b.Text != "" {
				return b.Text
			}
		}
	}
	return ""
}

// 媒体探针载荷。都是最小合法字节，靠魔数即可被 SniffMediaType 认出，
// 从而在 media type 缺失时也走同一条判定。
var (
	pngPixel = base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n0123456789"))
	wavClip  = base64.StdEncoding.EncodeToString([]byte("RIFF0000WAVEfmt "))
	pdfDoc   = base64.StdEncoding.EncodeToString([]byte("%PDF-1.7\n%%EOF\n"))
	textFile = base64.StdEncoding.EncodeToString([]byte("plain notes"))
)

// --- 收尾矩阵 ---

// terminationCase 是一份在终止帧之前就断掉的上游流，按未闭合块的成色分类。
// 三类成色对应聚合器的三种判定：可安全收尾的文本、入参截断的工具调用、
// 无签名的推理块。
type terminationCase struct {
	name   string
	frames map[string]string
	// verify 同时拿到上游侧与客户端侧的聚合器：有些判定（块是否仍开着）
	// 只在上游侧成立，因为入站编码器收尾时会补齐闭合帧。
	verify func(t *testing.T, client string, up, down *ir.Aggregator, rendered string)
}

func terminationCases() []terminationCase {
	return []terminationCase{
		{
			// 半句文本：可接受的截断。补一个闭合帧不会毒化历史，
			// 但补出来的内容必须一个字都不多。
			name: "open-text",
			frames: map[string]string{
				codec.ProtocolAnthropic: strings.Join([]string{
					`event: message_start`,
					`data: {"type":"message_start","message":{"id":"msg_c","model":"native","role":"assistant","usage":{"input_tokens":10}}}`,
					``,
					`event: content_block_start`,
					`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
					``,
					`event: content_block_delta`,
					`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"half sen"}}`,
					``,
				}, "\n"),
				codec.ProtocolChatCompletions: strings.Join([]string{
					`data: {"id":"msg_c","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
					``,
					`data: {"id":"msg_c","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{"content":"half sen"}}]}`,
					``,
				}, "\n"),
				codec.ProtocolResponses: strings.Join([]string{
					`event: response.created`,
					`data: {"type":"response.created","response":{"id":"msg_c","model":"native","status":"in_progress"}}`,
					``,
					`event: response.output_item.added`,
					`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant"}}`,
					``,
					`event: response.content_part.added`,
					`data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text"}}`,
					``,
					`event: response.output_text.delta`,
					`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"half sen"}`,
					``,
				}, "\n"),
				codec.ProtocolGemini: strings.Join([]string{
					`data: {"responseId":"msg_c","modelVersion":"native","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"half sen"}]}}],"usageMetadata":{"promptTokenCount":10}}`,
					``,
				}, "\n"),
			},
			verify: func(t *testing.T, client string, up, down *ir.Aggregator, rendered string) {
				if bad := up.UnsafeToClose(); len(bad) != 0 {
					t.Errorf("半句文本应可安全收尾，实被判为 %v\n%s", bad, rendered)
				}
				if got := responseText(down.Response()); got != "half sen" {
					t.Errorf("文本 = %q，want %q（收尾不得凭空补造内容）\n%s", got, "half sen", rendered)
				}
			},
		},
		{
			// 入参截断：补闭合帧就等于把一条毒历史交给客户端，
			// 所以这类必须能被判定出来，并且在转换后仍然判得出来。
			name: "truncated-tool-args",
			frames: map[string]string{
				codec.ProtocolAnthropic: strings.Join([]string{
					`event: message_start`,
					`data: {"type":"message_start","message":{"id":"msg_c","model":"native","role":"assistant","usage":{"input_tokens":10}}}`,
					``,
					`event: content_block_start`,
					`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_9","name":"grep","input":{}}}`,
					``,
					`event: content_block_delta`,
					`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"pattern\":"}}`,
					``,
				}, "\n"),
				codec.ProtocolChatCompletions: strings.Join([]string{
					`data: {"id":"msg_c","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"grep","arguments":"{\"pattern\":"}}]}}]}`,
					``,
				}, "\n"),
				codec.ProtocolResponses: strings.Join([]string{
					`event: response.created`,
					`data: {"type":"response.created","response":{"id":"msg_c","model":"native","status":"in_progress"}}`,
					``,
					`event: response.output_item.added`,
					`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_9","name":"grep"}}`,
					``,
					`event: response.function_call_arguments.delta`,
					`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"pattern\":"}`,
					``,
				}, "\n"),
				// gemini 缺席：args 一次到齐，表达不出截断，写不出 fixture 的格
				// 不进矩阵。
			},
			verify: func(t *testing.T, client string, up, down *ir.Aggregator, rendered string) {
				if len(up.IncompleteTools()) == 0 {
					t.Fatalf("上游侧未判出截断入参\n%s", rendered)
				}
				if len(up.UnsafeToClose()) == 0 {
					t.Errorf("截断入参应属不可安全收尾\n%s", rendered)
				}
				if len(down.IncompleteTools()) == 0 {
					t.Errorf("截断入参穿过转换后不再判得出来，客户端会把残缺调用存进历史\n%s", rendered)
				}
			},
		},
		{
			// 无签名的推理块：回传给上游会被判为伪造而整轮拒收。
			// 各上游解码器的 Finish 行为不同——有的补闭合帧有的不补——
			// 所以断言写成蕴含式，两种行为下都成立。
			name: "open-thinking",
			frames: map[string]string{
				codec.ProtocolAnthropic: strings.Join([]string{
					`event: message_start`,
					`data: {"type":"message_start","message":{"id":"msg_c","model":"native","role":"assistant","usage":{"input_tokens":10}}}`,
					``,
					`event: content_block_start`,
					`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
					``,
					`event: content_block_delta`,
					`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"pondering"}}`,
					``,
				}, "\n"),
				codec.ProtocolChatCompletions: strings.Join([]string{
					`data: {"id":"msg_c","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
					``,
					`data: {"id":"msg_c","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{"reasoning_content":"pondering"}}]}`,
					``,
				}, "\n"),
				codec.ProtocolResponses: strings.Join([]string{
					`event: response.created`,
					`data: {"type":"response.created","response":{"id":"msg_c","model":"native","status":"in_progress"}}`,
					``,
					`event: response.output_item.added`,
					`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning"}}`,
					``,
					`event: response.reasoning_summary_text.delta`,
					`data: {"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"pondering"}`,
					``,
				}, "\n"),
				codec.ProtocolGemini: strings.Join([]string{
					`data: {"responseId":"msg_c","modelVersion":"native","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"pondering","thought":true}]}}],"usageMetadata":{"promptTokenCount":10}}`,
					``,
				}, "\n"),
			},
			verify: func(t *testing.T, client string, up, down *ir.Aggregator, rendered string) {
				if up.HasOpenBlocks() && len(up.UnsafeToClose()) == 0 {
					t.Errorf("推理块仍开着且无签名，应属不可安全收尾\n%s", rendered)
				}
				if got := thinkingText(down.Response()); got != "pondering" {
					t.Errorf("推理文本 = %q，want %q\n%s", got, "pondering", rendered)
				}
			},
		},
	}
}

// TestTerminationMatrixClosesOutCleanly 是收尾矩阵：三类成色 × 4 上游 × 3 客户端。
// 每格的共同要求是「客户端一定拿到合法终止标志」——断流不能让客户端挂在
// 半开的流上等超时；各成色自己的要求由 verify 补充。
func TestTerminationMatrixClosesOutCleanly(t *testing.T) {
	for _, tc := range terminationCases() {
		for _, up := range outboundNames() {
			raw, ok := tc.frames[up]
			if !ok {
				continue
			}
			for _, client := range inboundNames() {
				t.Run(tc.name+"/"+up+"→"+client, func(t *testing.T) {
					rendered := renderStream(t, client, decodeStream(t, up, raw))
					if want := terminator(client); !strings.Contains(rendered, want) {
						t.Fatalf("断流后客户端未收到终止标志 %q\n%s", want, rendered)
					}
					tc.verify(t, client, aggregator(t, up, raw), aggregator(t, client, rendered), rendered)
				})
			}
		}
	}
}

// --- 不可表达的格 ---

// protocolLimitation 记录一个因协议本身表达不出而不进矩阵的格。
//
// 这类格不写 t.Skip：Skip 需要一个已经跑起来的子测试，而这里连 fixture 都
// 写不出来——凭空造一份不存在的线上形态，测的就不是真实协议了。放宽断言更糟，
// 会把「协议本就如此」记成通过。所以做法是让格缺席，并在此登记书面理由，
// 再由下面的守卫测试保证缺席与登记严格一一对应：新增协议忘了写 fixture 会
// 被判为未登记的缺口，协议后来长出该能力则登记会被判为过期。
type protocolLimitation struct {
	matrix   string
	protocol string
	feature  string
	reason   string
	// absent 回答该格此刻是否确实缺席。
	absent func() bool
}

func protocolLimitations() []protocolLimitation {
	return []protocolLimitation{
		{
			matrix: "stop_reason", protocol: codec.ProtocolChatCompletions, feature: "stop_sequence",
			reason: "finish_reason 无专门取值，命中停止序列并入 stop（正常结束）",
			absent: func() bool { return !hasStopWire(codec.ProtocolChatCompletions, ir.StopStopSequence) },
		},
		{
			matrix: "stop_reason", protocol: codec.ProtocolResponses, feature: "stop_sequence",
			reason: "status 只分 completed/incomplete，命中停止序列落在 completed",
			absent: func() bool { return !hasStopWire(codec.ProtocolResponses, ir.StopStopSequence) },
		},
		{
			matrix: "stop_reason", protocol: codec.ProtocolGemini, feature: "stop_sequence",
			reason: "finishReason 无对应枚举值，命中停止序列报 STOP",
			absent: func() bool { return !hasStopWire(codec.ProtocolGemini, ir.StopStopSequence) },
		},
		{
			matrix: "malformed-stream", protocol: codec.ProtocolGemini, feature: "truncated tool args",
			reason: "functionCall 的 args 一帧到齐，不分片，故表达不出入参截断",
			absent: func() bool { return !hasMalformedStream("tool-args-truncated", codec.ProtocolGemini) },
		},
		{
			matrix: "malformed-stream", protocol: codec.ProtocolAnthropic, feature: "args before name",
			reason: "content_block_start 必带 name，入参不可能先于名字抵达",
			absent: func() bool { return !hasMalformedStream("tool-args-before-name", codec.ProtocolAnthropic) },
		},
		{
			matrix: "malformed-stream", protocol: codec.ProtocolResponses, feature: "args before name",
			reason: "output_item.added 必带 name，同上",
			absent: func() bool { return !hasMalformedStream("tool-args-before-name", codec.ProtocolResponses) },
		},
		{
			matrix: "malformed-stream", protocol: codec.ProtocolGemini, feature: "args before name",
			reason: "functionCall 是整体对象，name 与 args 同帧",
			absent: func() bool { return !hasMalformedStream("tool-args-before-name", codec.ProtocolGemini) },
		},
		{
			matrix: "termination", protocol: codec.ProtocolGemini, feature: "truncated tool args",
			reason: "同上：args 不分片，断流断不出半截入参",
			absent: func() bool { return !hasTerminationCase("truncated-tool-args", codec.ProtocolGemini) },
		},
		{
			matrix: "usage-reasoning", protocol: codec.ProtocolAnthropic, feature: "reasoning tokens",
			reason: "usage 结构无推理计量字段",
			absent: func() bool { _, ok := reasoningUsageFixture[codec.ProtocolAnthropic]; return !ok },
		},
		{
			matrix: "usage-cache-write", protocol: codec.ProtocolResponses, feature: "cache write tokens",
			reason: "input_tokens_details 只有 cached_tokens，无缓存写入计量",
			absent: func() bool { _, ok := cacheWriteUsageFixture[codec.ProtocolResponses]; return !ok },
		},
		{
			matrix: "usage-cache-write", protocol: codec.ProtocolGemini, feature: "cache write tokens",
			reason: "usageMetadata 只有 cachedContentTokenCount，无缓存写入计量",
			absent: func() bool { _, ok := cacheWriteUsageFixture[codec.ProtocolGemini]; return !ok },
		},
		{
			matrix: "same-id-across-index", protocol: codec.ProtocolAnthropic, feature: "duplicate index",
			reason: "块身份只由 index 表达，id 在 content_block_start 出现一次且不随增量重复，" +
				"同一调用的分片不可能带不同 index",
			absent: func() bool { _, ok := sameIDAcrossIndexStreams[codec.ProtocolAnthropic]; return !ok },
		},
		{
			matrix: "same-id-across-index", protocol: codec.ProtocolResponses, feature: "duplicate index",
			reason: "定位靠 output_index，call_id 只在 output_item.added 出现，增量帧不带 id，" +
				"两套标识不并行",
			absent: func() bool { _, ok := sameIDAcrossIndexStreams[codec.ProtocolResponses]; return !ok },
		},
		{
			matrix: "same-id-across-index", protocol: codec.ProtocolGemini, feature: "duplicate index",
			reason: "functionCall 是整体对象、args 一帧到齐，不存在分片，也就无从跨 index 抵达",
			absent: func() bool { _, ok := sameIDAcrossIndexStreams[codec.ProtocolGemini]; return !ok },
		},
		{
			matrix: "synth-id-omission", protocol: codec.ProtocolAnthropic, feature: "omitted id",
			reason: "tool_use.id 与 tool_result.tool_use_id 都是必填，省略后上游无从把结果" +
				"回指到调用，整轮请求被拒——比发一个它没见过的 id 更糟",
			absent: func() bool { return !toolIDOptional(codec.ProtocolAnthropic) },
		},
		{
			matrix: "synth-id-omission", protocol: codec.ProtocolChatCompletions, feature: "omitted id",
			reason: "tool_calls[].id 与 tool_call_id 都是必填，缺了上游无法配对",
			absent: func() bool { return !toolIDOptional(codec.ProtocolChatCompletions) },
		},
		{
			matrix: "synth-id-omission", protocol: codec.ProtocolResponses, feature: "omitted id",
			reason: "call_id 是 function_call 与 function_call_output 之间唯一的配对键，" +
				"省略等于把这次调用与它的结果彻底断开",
			absent: func() bool { return !toolIDOptional(codec.ProtocolResponses) },
		},
		{
			matrix: "error-shape", protocol: codec.ProtocolChatCompletions, feature: "block close",
			reason: "本协议的流式形态是扁平的 choices[].delta，没有块生命周期，" +
				"也就没有可悬在半开状态的块，无需闭合帧",
			absent: func() bool { _, ok := blockCloseFrame[codec.ProtocolChatCompletions]; return !ok },
		},
		{
			matrix: "error-param", protocol: codec.ProtocolAnthropic, feature: "param",
			reason: "本协议的错误信封只有 {type,message} 两个位，param 无处安放",
			absent: func() bool { return inboundDropsParam(codec.ProtocolAnthropic) },
		},
	}
}

// toolIDOptional 取该出站协议的 id 可选性。
// 从能力位读而非写死协议名：新协议进注册表即自动进这套判定。
func toolIDOptional(protocol string) bool {
	c, ok := codec.Outbound(protocol)
	if !ok {
		return false
	}
	return c.Caps().ToolIDOptional
}

// TestProtocolLimitationsAreStillTrue 守卫每条登记：格必须真的缺席。
// 登记的格若冒出了 fixture，说明协议已能表达，该把它并回矩阵而不是留着豁免。
func TestProtocolLimitationsAreStillTrue(t *testing.T) {
	for _, lim := range protocolLimitations() {
		t.Run(lim.matrix+"/"+lim.protocol+"/"+lim.feature, func(t *testing.T) {
			if lim.reason == "" {
				t.Fatal("协议限制必须写明理由")
			}
			if !lim.absent() {
				t.Errorf("该格已有 fixture，登记的限制已过期，应并回矩阵：%s", lim.reason)
			}
		})
	}
}

// TestEveryMatrixGapIsDocumented 反向守卫：矩阵里每处缺席都得有登记。
// 没这条的话，新增协议时忘写 fixture 会安静地少测一格。
func TestEveryMatrixGapIsDocumented(t *testing.T) {
	documented := map[string]bool{}
	for _, lim := range protocolLimitations() {
		documented[lim.matrix+"/"+lim.protocol+"/"+lim.feature] = true
	}
	assert := func(t *testing.T, matrix, protocol, feature string) {
		t.Helper()
		if !documented[matrix+"/"+protocol+"/"+feature] {
			t.Errorf("%s 矩阵缺 %s 的 %s 格，且未登记协议限制", matrix, protocol, feature)
		}
	}

	for _, up := range outboundNames() {
		if !hasStopWire(up, ir.StopStopSequence) {
			assert(t, "stop_reason", up, "stop_sequence")
		}
		if !hasMalformedStream("tool-args-truncated", up) {
			assert(t, "malformed-stream", up, "truncated tool args")
		}
		if !hasMalformedStream("tool-args-before-name", up) {
			assert(t, "malformed-stream", up, "args before name")
		}
		if !hasTerminationCase("truncated-tool-args", up) {
			assert(t, "termination", up, "truncated tool args")
		}
		if _, ok := reasoningUsageFixture[up]; !ok {
			assert(t, "usage-reasoning", up, "reasoning tokens")
		}
		if _, ok := cacheWriteUsageFixture[up]; !ok {
			assert(t, "usage-cache-write", up, "cache write tokens")
		}
		if _, ok := sameIDAcrossIndexStreams[up]; !ok {
			assert(t, "same-id-across-index", up, "duplicate index")
		}
		if !toolIDOptional(up) {
			assert(t, "synth-id-omission", up, "omitted id")
		}
	}

	// 错误路径的两组矩阵按入站协议展开：错误渲染是入站职责。
	for _, in := range codec.InboundNames() {
		if _, ok := blockCloseFrame[in]; !ok {
			assert(t, "error-shape", in, "block close")
		}
		if inboundDropsParam(in) {
			assert(t, "error-param", in, "param")
		}
	}
}

func hasStopWire(protocol string, reason ir.StopReason) bool {
	_, ok := upstreamStopWire[protocol][reason]
	return ok
}

func hasMalformedStream(name, protocol string) bool {
	for _, fx := range malformedStreams {
		if fx.name != name {
			continue
		}
		_, ok := fx.frames[protocol]
		return ok
	}
	return false
}

func hasTerminationCase(name, protocol string) bool {
	for _, tc := range terminationCases() {
		if tc.name != name {
			continue
		}
		_, ok := tc.frames[protocol]
		return ok
	}
	return false
}

func responseText(resp *ir.Response) string {
	var b strings.Builder
	for _, blk := range resp.Content {
		if blk.Type == ir.BlockText {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

func thinkingText(resp *ir.Response) string {
	var b strings.Builder
	for _, blk := range resp.Content {
		if blk.Type == ir.BlockThinking && blk.Thinking != nil {
			b.WriteString(blk.Thinking.Text)
		}
	}
	return b.String()
}
