package codec_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 本文件覆盖第二十三轮的四个响应侧语义维度：停止序列身份、工具结果失败态、
// 响应内媒体的丢弃说明、拒答的终止原因。
//
// 四者的共同形态是「上游给了一个语义维度，而中立表示或出站编码没有承载它的
// 位置」，都是 HTTP 200 下的静默语义损坏，因此各自都要有「触发」与「未触发」
// 两条断言——只测触发路径的话，一个无条件生效的实现同样能通过。

// --- 需求1：停止序列的身份 ---

// TestAnthropicNonStreamCarriesStopSequence 非流式往返：上游给的那条序列
// 必须原样到达 IR，再原样写回客户端。
func TestAnthropicNonStreamCarriesStopSequence(t *testing.T) {
	oc, ok := codec.Outbound(codec.ProtocolAnthropic)
	if !ok {
		t.Fatal("anthropic outbound not registered")
	}
	resp, err := oc.DecodeResponse([]byte(`{"id":"msg_1","model":"m",` +
		`"content":[{"type":"text","text":"a"}],"stop_reason":"stop_sequence",` +
		`"stop_sequence":"\n\nHuman:","usage":{"input_tokens":3,"output_tokens":4}}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StopReason != ir.StopStopSequence {
		t.Fatalf("stop_reason = %q，想要 %q", resp.StopReason, ir.StopStopSequence)
	}
	if resp.StopSequence != "\n\nHuman:" {
		t.Fatalf("StopSequence = %q，上游给的那条序列没有到达 IR", resp.StopSequence)
	}

	body, notes := responseLossyOf(t, codec.ProtocolAnthropic, func() *ir.Response {
		return &ir.Response{ID: "msg_1", Model: "m", StopReason: ir.StopStopSequence,
			StopSequence: "\n\nHuman:", Content: []ir.Block{{Type: ir.BlockText, Text: "a"}}}
	})
	var probe struct {
		StopReason   string `json:"stop_reason"`
		StopSequence string `json:"stop_sequence"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	if probe.StopSequence != "\n\nHuman:" {
		t.Errorf("出站没有写回 stop_sequence：%s", body)
	}
	// 同族协议有原生字段，不该被报成损失。
	if hasNoteWith(notes, "stop_sequence") {
		t.Errorf("anthropic 原生承载这一维，不该报有损：%v", notes)
	}
}

// TestAnthropicStreamCarriesStopSequence 流式往返：这一维随收尾帧抵达，
// 再随 message_delta 写回。
func TestAnthropicStreamCarriesStopSequence(t *testing.T) {
	raw := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"m","content":[],"usage":{"input_tokens":3}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"stop_sequence","stop_sequence":"END"},"usage":{"output_tokens":4}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	resp, _ := decodeStreamWithNotes(t, codec.ProtocolAnthropic, raw)
	if resp.StopSequence != "END" {
		t.Fatalf("流式 StopSequence = %q，想要 END", resp.StopSequence)
	}

	// 再编回流：message_delta 必须带上它。
	out, _ := renderStreamWithNotes(t, codec.ProtocolAnthropic, ir.ResponseEvents(resp))
	if !strings.Contains(out, `"stop_sequence":"END"`) {
		t.Errorf("流式出站的 message_delta 未写回 stop_sequence：%s", out)
	}
}

// TestStopSequenceNotAdoptedOnOtherStopReason 互斥约束：上游给了序列但终止
// 原因不是它时不得采纳。回填一条未触发的序列会让按它分段的客户端切错位置。
func TestStopSequenceNotAdoptedOnOtherStopReason(t *testing.T) {
	oc, _ := codec.Outbound(codec.ProtocolAnthropic)
	resp, err := oc.DecodeResponse([]byte(`{"id":"msg_1","model":"m","content":[],` +
		`"stop_reason":"end_turn","stop_sequence":"END","usage":{}}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StopSequence != "" {
		t.Errorf("stop_reason=end_turn 时不得采纳序列，got %q", resp.StopSequence)
	}

	raw := "event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens","stop_sequence":"END"},"usage":{"output_tokens":4}}` +
		"\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	stream, _ := decodeStreamWithNotes(t, codec.ProtocolAnthropic, raw)
	if stream.StopSequence != "" {
		t.Errorf("流式 stop_reason=max_tokens 时不得采纳序列，got %q", stream.StopSequence)
	}

	// 出站同样要守：IR 里万一带着矛盾的一对，写出去仍不能带序列。
	body, _ := responseLossyOf(t, codec.ProtocolAnthropic, func() *ir.Response {
		return &ir.Response{ID: "msg_1", Model: "m", StopReason: ir.StopEndTurn,
			StopSequence: "END", Content: []ir.Block{}}
	})
	if strings.Contains(string(body), "stop_sequence") {
		t.Errorf("出站在 stop_reason 不匹配时写出了 stop_sequence：%s", body)
	}
	out, _ := renderStreamWithNotes(t, codec.ProtocolAnthropic, []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "msg_1", Model: "m"},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, StopSequence: "END", Usage: &ir.Usage{}},
		{Type: ir.EvMessageStop},
	})
	if strings.Contains(out, "stop_sequence") {
		t.Errorf("流式出站在 stop_reason 不匹配时写出了 stop_sequence：%s", out)
	}
}

// TestOtherInboundsOmitStopSequence 其余三协议的线上形态没有这一维：
// 不合成、不报有损。合成一个它们的客户端不读的键只会增大响应体。
func TestOtherInboundsOmitStopSequence(t *testing.T) {
	for _, name := range inboundNames() {
		if name == codec.ProtocolAnthropic {
			continue
		}
		t.Run(name, func(t *testing.T) {
			body, notes := responseLossyOf(t, name, func() *ir.Response {
				return &ir.Response{ID: "msg_1", Model: "m", StopReason: ir.StopStopSequence,
					StopSequence: "END", Content: []ir.Block{{Type: ir.BlockText, Text: "a"}}}
			})
			if strings.Contains(string(body), "stop_sequence") {
				t.Errorf("%s 不该合成 stop_sequence：%s", name, body)
			}
			if strings.Contains(string(body), "END") {
				t.Errorf("%s 把序列原文塞进了响应体：%s", name, body)
			}
			if hasNoteWith(notes, "stop_sequence") {
				t.Errorf("%s 不该为协议本身没有的键报有损：%v", name, notes)
			}
		})
	}
}

// TestStopSequenceWireKeys 钉住键名字面量：改名会静默改变协议形状。
// 这是第六次同型缺口，每轮都要钉。
func TestStopSequenceWireKeys(t *testing.T) {
	raw, err := json.Marshal(ir.Response{StopReason: ir.StopStopSequence, StopSequence: "END"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"stop_sequence":"END"`) {
		t.Errorf("ir.Response 的键名应为 stop_sequence，got %s", raw)
	}
	ev, err := json.Marshal(ir.Event{Type: ir.EvMessageDelta, StopSequence: "END"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ev), `"stop_sequence":"END"`) {
		t.Errorf("ir.Event 的键名应为 stop_sequence，got %s", ev)
	}
	// 零值必须缺席：绝大多数响应不由停止序列结束，多一个空字段会让
	// 「未触发即字节不变」不再成立。
	plain, err := json.Marshal(ir.Response{StopReason: ir.StopEndTurn})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "stop_sequence") {
		t.Errorf("零值不该出现 stop_sequence，got %s", plain)
	}
}

// TestResponseEventsProjectionKeepsStopSequence 非流式→流式投影必须带上
// 这一维：数据面对上游一律流式，但上游可能回整份 JSON，那条路径要等价。
func TestResponseEventsProjectionKeepsStopSequence(t *testing.T) {
	events := ir.ResponseEvents(&ir.Response{ID: "msg_1", Model: "m",
		StopReason: ir.StopStopSequence, StopSequence: "END"})
	var seen bool
	for _, ev := range events {
		if ev.Type != ir.EvMessageDelta {
			continue
		}
		seen = true
		if ev.StopSequence != "END" {
			t.Errorf("投影丢了 StopSequence：%+v", ev)
		}
	}
	if !seen {
		t.Fatal("投影里没有 message_delta 事件")
	}
}

// --- 需求2：工具结果的失败态 ---

// toolResultReq 造一份带工具结果的请求，IsError 由入参决定。
func toolResultReq(isErr bool) *ir.Request {
	return &ir.Request{
		Model:     "native",
		MaxTokens: 64,
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "run it"}}},
			{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockToolUse,
				ToolUse: &ir.ToolUse{ID: "call_1", Name: "grep", Input: `{"q":"x"}`}}}},
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockToolResult,
				ToolResult: &ir.ToolResult{ToolUseID: "call_1", IsError: isErr,
					Content: []ir.Block{{Type: ir.BlockText, Text: "exit 1"}}}}}},
		},
		Tools: []ir.Tool{{Name: "grep", Schema: `{"type":"object"}`}},
	}
}

// TestToolResultErrorPrefixedWhereUnsupported 两个无原生失败标记的协议必须
// 把失败态改写成内容前缀，并报说明。丢掉它会让模型把失败当成功。
func TestToolResultErrorPrefixedWhereUnsupported(t *testing.T) {
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses} {
		t.Run(name, func(t *testing.T) {
			if capsOf(t, name).ToolResultError {
				t.Fatalf("%s 声明了原生失败标记，本用例的前提不再成立", name)
			}
			body, notes := lossyOf(t, name, toolResultReq(true))
			if !strings.Contains(string(body), codec.ToolErrorPrefix()) {
				t.Errorf("%s 未在工具结果前加失败前缀：%s", name, body)
			}
			if !hasNoteWith(notes, "tool_result.is_error") {
				t.Errorf("%s 加了前缀却不报说明：%v", name, notes)
			}
			// 措辞必须是 rewrote 而非 dropped：读者的下一步动作不同。
			if !hasNoteWith(notes, "rewrote") {
				t.Errorf("%s 的说明应为改写而非丢弃：%v", name, notes)
			}
		})
	}
}

// TestToolResultSuccessUnchanged 未触发路径：IsError 为假时字节完全不变、
// 不报说明。这条挡住「无条件加前缀」那一类实现。
func TestToolResultSuccessUnchanged(t *testing.T) {
	for _, name := range outboundNames() {
		t.Run(name, func(t *testing.T) {
			body, notes := lossyOf(t, name, toolResultReq(false))
			if strings.Contains(string(body), codec.ToolErrorPrefix()) {
				t.Errorf("%s 在 IsError 为假时加了前缀：%s", name, body)
			}
			if hasNoteWith(notes, "tool_result.is_error") {
				t.Errorf("%s 在 IsError 为假时报了失败态说明：%v", name, notes)
			}
		})
	}
}

// TestNativeToolResultErrorNotRewritten 两个有原生表达的协议行为不变：
// 不加前缀、不报说明。
func TestNativeToolResultErrorNotRewritten(t *testing.T) {
	for _, name := range []string{codec.ProtocolAnthropic, codec.ProtocolGemini} {
		t.Run(name, func(t *testing.T) {
			if !capsOf(t, name).ToolResultError {
				t.Fatalf("%s 应声明原生失败标记", name)
			}
			body, notes := lossyOf(t, name, toolResultReq(true))
			if strings.Contains(string(body), codec.ToolErrorPrefix()) {
				t.Errorf("%s 有原生失败标记，不该加前缀：%s", name, body)
			}
			if hasNoteWith(notes, "tool_result.is_error") {
				t.Errorf("%s 有原生失败标记，不该报说明：%v", name, notes)
			}
			// 原生表达仍要在字节里：anthropic 的 is_error、gemini 的 error 键。
			want := "is_error"
			if name == codec.ProtocolGemini {
				want = `"error"`
			}
			if !strings.Contains(string(body), want) {
				t.Errorf("%s 未写出原生失败表达 %s：%s", name, want, body)
			}
		})
	}
}

// TestToolErrorPrefixSingleSource 前缀措辞只有一个出处：两个出站各自写一份
// 字面量会在改措辞时只改一处，而说明里报的是另一份。
func TestToolErrorPrefixSingleSource(t *testing.T) {
	if codec.ToolErrorPrefix() != "[tool error] " {
		t.Errorf("前缀措辞变了：%q", codec.ToolErrorPrefix())
	}
	var bodies []string
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses} {
		body, _ := lossyOf(t, name, toolResultReq(true))
		bodies = append(bodies, string(body))
	}
	for i, b := range bodies {
		if !strings.Contains(b, "[tool error] exit 1") {
			t.Errorf("出站 %d 的前缀未紧贴原内容：%s", i, b)
		}
	}
}

// TestPrefixToolResultErrorDoesNotMutateInput 前缀不得原地改 IR：同一份请求
// 会被两条编码路径各走一遍，改了入参第二遍就带上两个前缀。
func TestPrefixToolResultErrorDoesNotMutateInput(t *testing.T) {
	result := &ir.ToolResult{ToolUseID: "call_1", IsError: true,
		Content: []ir.Block{{Type: ir.BlockText, Text: "exit 1"}}}
	caps := codec.Capabilities{}
	first := codec.PrefixToolResultError(result, caps)
	if len(result.Content) != 1 || result.Content[0].Text != "exit 1" {
		t.Fatalf("入参被原地改了：%+v", result.Content)
	}
	second := codec.PrefixToolResultError(result, caps)
	if len(first) != len(second) {
		t.Errorf("两次调用结果长度不同：%d vs %d", len(first), len(second))
	}
}

// --- 需求3：被丢弃的响应内媒体 ---

const geminiMediaStream = "data: " +
	`{"responseId":"msg_1","modelVersion":"native","candidates":[{"index":0,"content":` +
	`{"role":"model","parts":[{"text":"here"},{"inlineData":{"mimeType":"image/png","data":"AA=="}}]}}]}` +
	"\n\ndata: " + `{"candidates":[{"index":0,"finishReason":"STOP"}]}` + "\n\n"

const geminiMediaBody = `{"responseId":"msg_1","modelVersion":"native","candidates":[{"index":0,` +
	`"content":{"role":"model","parts":[{"text":"here"},` +
	`{"inlineData":{"mimeType":"image/png","data":"AA=="}}]},"finishReason":"STOP"}]}`

// TestGeminiStreamReportsDroppedResponseMedia 流式路径：丢了图要说出来。
func TestGeminiStreamReportsDroppedResponseMedia(t *testing.T) {
	resp, notes := decodeStreamWithNotes(t, codec.ProtocolGemini, geminiMediaStream)
	if !hasNoteWith(notes, "image/png") {
		t.Errorf("流式丢弃响应内媒体未报 media type：%v", notes)
	}
	// 文字仍要在：丢的只是媒体那一个 part。
	if !responseHasText(resp, "here") {
		t.Errorf("同一 content 里的文字被一起丢了：%+v", resp.Content)
	}
}

// TestGeminiNonStreamReportsDroppedResponseMedia 非流式路径此前连分支都没有，
// 媒体 part 落到 switch 外面被静默跳过。
func TestGeminiNonStreamReportsDroppedResponseMedia(t *testing.T) {
	oc, ok := codec.Outbound(codec.ProtocolGemini)
	if !ok {
		t.Fatal("gemini outbound not registered")
	}
	ld, ok := oc.(codec.LossyResponseDecoder)
	if !ok {
		t.Fatal("gemini 必须实现 LossyResponseDecoder")
	}
	resp, notes, err := ld.DecodeResponseLossy([]byte(geminiMediaBody))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !hasNoteWith(notes, "image/png") {
		t.Errorf("非流式丢弃响应内媒体未报 media type：%v", notes)
	}
	if !responseHasText(resp, "here") {
		t.Errorf("非流式把同 content 的文字一起丢了：%+v", resp.Content)
	}
}

// TestGeminiDroppedMediaNoteSameWording 两处措辞必须同一出处：不同措辞会让
// 按说明聚合的排查工具把同一件事记成两类。
func TestGeminiDroppedMediaNoteSameWording(t *testing.T) {
	_, streamNotes := decodeStreamWithNotes(t, codec.ProtocolGemini, geminiMediaStream)
	oc, _ := codec.Outbound(codec.ProtocolGemini)
	ld := oc.(codec.LossyResponseDecoder)
	_, plainNotes, err := ld.DecodeResponseLossy([]byte(geminiMediaBody))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	s := noteWith(streamNotes, "image/png")
	p := noteWith(plainNotes, "image/png")
	if s == "" || p == "" {
		t.Fatalf("两处都要有说明：stream=%v plain=%v", streamNotes, plainNotes)
	}
	if s != p {
		t.Errorf("两处措辞不同：\n stream: %s\n plain:  %s", s, p)
	}
}

// TestGeminiNoMediaNoNote 未触发路径：没有媒体块时不报说明。
func TestGeminiNoMediaNoNote(t *testing.T) {
	clean := "data: " + `{"responseId":"msg_1","modelVersion":"native","candidates":` +
		`[{"index":0,"content":{"role":"model","parts":[{"text":"here"}]},"finishReason":"STOP"}]}` + "\n\n"
	_, notes := decodeStreamWithNotes(t, codec.ProtocolGemini, clean)
	if hasNoteWith(notes, "from the response") {
		t.Errorf("无媒体块却报了丢弃说明：%v", notes)
	}
	oc, _ := codec.Outbound(codec.ProtocolGemini)
	ld := oc.(codec.LossyResponseDecoder)
	_, plain, err := ld.DecodeResponseLossy([]byte(`{"responseId":"msg_1","modelVersion":"native",` +
		`"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"here"}]},"finishReason":"STOP"}]}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if hasNoteWith(plain, "from the response") {
		t.Errorf("非流式无媒体块却报了丢弃说明：%v", plain)
	}
}

// --- 需求4：拒答不得被降级成正常结束 ---

// TestResponsesRefusalBecomesContentFilter 非流式：refusal part 在场时终止
// 原因必须是 content_filter，不是 end_turn。
func TestResponsesRefusalBecomesContentFilter(t *testing.T) {
	oc, ok := codec.Outbound(codec.ProtocolResponses)
	if !ok {
		t.Fatal("responses outbound not registered")
	}
	resp, err := oc.DecodeResponse([]byte(`{"id":"resp_1","model":"m","status":"completed",` +
		`"output":[{"type":"message","role":"assistant","content":` +
		`[{"type":"refusal","refusal":"I cannot help with that."}]}]}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StopReason != ir.StopContentFilter {
		t.Errorf("stop_reason = %q，想要 %q", resp.StopReason, ir.StopContentFilter)
	}
	// 拒绝正文进独立的 refusal 块（不再并入文本块）：客户端要看到拒答
	// 说了什么，且要能区分「模型拒绝了」与「模型这么答的」。
	if !responseHasRefusal(resp, "I cannot help with that.") {
		t.Errorf("拒答文字没有落进 refusal 块：%+v", resp.Content)
	}
}

// TestResponsesStreamRefusalBecomesContentFilter 流式：只有 refusal.delta 到过，
// 收尾帧的 status 仍是 completed。
func TestResponsesStreamRefusalBecomesContentFilter(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"m"}}`,
		``,
		`data: {"type":"response.refusal.delta","output_index":0,"content_index":0,"delta":"I cannot"}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed"}}`,
		``,
	}, "\n")
	resp, _ := decodeStreamWithNotes(t, codec.ProtocolResponses, raw)
	if resp.StopReason != ir.StopContentFilter {
		t.Errorf("流式 stop_reason = %q，想要 %q", resp.StopReason, ir.StopContentFilter)
	}
	if !responseHasRefusal(resp, "I cannot") {
		t.Errorf("流式拒答文字没有落进 refusal 块：%+v", resp.Content)
	}
}

// TestResponsesStreamRefusalPartWithoutDelta part 开启帧带完整拒答文字、
// 不发 delta 的实现同样要被认出来。
func TestResponsesStreamRefusalPartWithoutDelta(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"m"}}`,
		``,
		`data: {"type":"response.content_part.added","output_index":0,"content_index":0,` +
			`"part":{"type":"refusal","refusal":"no"}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed"}}`,
		``,
	}, "\n")
	resp, _ := decodeStreamWithNotes(t, codec.ProtocolResponses, raw)
	if resp.StopReason != ir.StopContentFilter {
		t.Errorf("stop_reason = %q，想要 %q", resp.StopReason, ir.StopContentFilter)
	}
}

// TestIncompleteDetailsBeatsRefusal 上游已明说没完成时以它为准：
// 那是比我方从 part 类型推断更可靠的表态。
func TestIncompleteDetailsBeatsRefusal(t *testing.T) {
	oc, _ := codec.Outbound(codec.ProtocolResponses)
	resp, err := oc.DecodeResponse([]byte(`{"id":"resp_1","model":"m","status":"incomplete",` +
		`"incomplete_details":{"reason":"max_output_tokens"},` +
		`"output":[{"type":"message","role":"assistant","content":` +
		`[{"type":"refusal","refusal":"no"}]}]}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StopReason != ir.StopMaxTokens {
		t.Errorf("stop_reason = %q，incomplete_details 应优先", resp.StopReason)
	}
}

// TestRefusalBeatsToolUse 同时出现拒答与工具调用时拒答胜：判成 tool_use
// 会让客户端去执行工具，而模型其实是拒绝了。
func TestRefusalBeatsToolUse(t *testing.T) {
	oc, _ := codec.Outbound(codec.ProtocolResponses)
	resp, err := oc.DecodeResponse([]byte(`{"id":"resp_1","model":"m","status":"completed",` +
		`"output":[{"type":"message","role":"assistant","content":` +
		`[{"type":"refusal","refusal":"no"}]},` +
		`{"type":"function_call","call_id":"call_1","name":"grep","arguments":"{}"}]}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StopReason != ir.StopContentFilter {
		t.Errorf("stop_reason = %q，拒答应胜过工具调用", resp.StopReason)
	}
}

// TestNoRefusalKeepsStopReason 未触发路径：没有 refusal 时判定不变。
func TestNoRefusalKeepsStopReason(t *testing.T) {
	oc, _ := codec.Outbound(codec.ProtocolResponses)
	cases := []struct {
		name string
		body string
		want ir.StopReason
	}{
		{"text", `{"id":"r","model":"m","status":"completed","output":[{"type":"message",` +
			`"role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}`, ir.StopEndTurn},
		{"tool", `{"id":"r","model":"m","status":"completed","output":[{"type":"function_call",` +
			`"call_id":"c","name":"grep","arguments":"{}"}]}`, ir.StopToolUse},
		// content 是字符串形态时解不成 part 数组，那不是错误也不是拒答。
		{"string-content", `{"id":"r","model":"m","status":"completed","output":[{"type":"message",` +
			`"role":"assistant","content":"hi"}]}`, ir.StopEndTurn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := oc.DecodeResponse([]byte(tc.body))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.StopReason != tc.want {
				t.Errorf("stop_reason = %q，想要 %q", resp.StopReason, tc.want)
			}
		})
	}
}

// TestStreamNoRefusalKeepsStopReason 流式未触发路径同型。
func TestStreamNoRefusalKeepsStopReason(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"m"}}`,
		``,
		`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hi"}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed"}}`,
		``,
	}, "\n")
	resp, _ := decodeStreamWithNotes(t, codec.ProtocolResponses, raw)
	if resp.StopReason != ir.StopEndTurn {
		t.Errorf("无拒答的流 stop_reason = %q，想要 %q", resp.StopReason, ir.StopEndTurn)
	}
}

// --- 共用辅助 ---

func responseHasText(resp *ir.Response, want string) bool {
	if resp == nil {
		return false
	}
	for _, b := range resp.Content {
		if b.Type == ir.BlockText && strings.Contains(b.Text, want) {
			return true
		}
	}
	return false
}

// responseHasRefusal 判定拒绝正文是否落进了独立的 refusal 块。
func responseHasRefusal(resp *ir.Response, want string) bool {
	if resp == nil {
		return false
	}
	for _, b := range resp.Content {
		if b.Type == ir.BlockRefusal && strings.Contains(b.Text, want) {
			return true
		}
	}
	return false
}

// noteWith 取出第一条含 sub 的说明原文，用于比对两处措辞是否同一份。
func noteWith(notes []string, sub string) string {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return n
		}
	}
	return ""
}
