package codec_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 本文件是响应侧的矩阵，与请求侧的 crossmatrix_test.go 对称。
// 分文件是因为两侧的维度不同（响应侧按客户端协议展开，请求侧按出站协议），
// 共用的只有注册表与 protocolLimitations 登记表。

// --- 9.1 响应侧字节稳定性辅助 ---

// responseLossyOf 走入站的有损响应编码，并顺带校验两条路径逐字节一致。
//
// 与请求侧 lossyOf 对等：诊断开关若能改变客户端看到的字节，排查动作本身
// 就成了故障变量——开着诊断复现不出来的问题会被记成偶发。
//
// 取 build 而不是取一个 *ir.Response：两条路径各要一份未被上一条动过的
// 输入，否则编码器万一原地改了块内容，第二条路径比的就不是同一件事。
func responseLossyOf(t *testing.T, in string, build func() *ir.Response) ([]byte, []string) {
	t.Helper()
	ic, ok := codec.Inbound(in)
	if !ok {
		t.Fatalf("inbound %q not registered", in)
	}
	le, ok := ic.(codec.LossyResponseEncoder)
	if !ok {
		t.Fatalf("inbound %q must implement LossyResponseEncoder", in)
	}
	body, notes, err := le.EncodeResponseLossy(build())
	if err != nil {
		t.Fatalf("%s EncodeResponseLossy: %v", in, err)
	}
	plain, err := ic.EncodeResponse(build())
	if err != nil {
		t.Fatalf("%s EncodeResponse: %v", in, err)
	}
	if string(body) != string(plain) {
		t.Fatalf("%s: 两条编码路径产出不同响应体\n lossy: %s\n plain: %s", in, body, plain)
	}
	return body, notes
}

// --- 9.2 零值序号字段矩阵 ---

// zeroIndexFields 是 responses 协议里各事件类型必须写出的序号字段。
//
// 只有这一个协议进矩阵：另外三个的序号字段在零值时缺席无害
// （anthropic/gemini 的 index 有默认值语义，chat_completions 的
// choices[].index 本就恒为 0 且由编码器写死），而 responses 的
// 消费方拿 output_index / content_index 当定位键，缺字段即定位失败。
var zeroIndexFields = map[string][]string{
	"response.output_item.added":             {"output_index"},
	"response.output_item.done":              {"output_index"},
	"response.content_part.added":            {"output_index", "content_index"},
	"response.content_part.done":             {"output_index", "content_index"},
	"response.output_text.delta":             {"output_index", "content_index"},
	"response.function_call_arguments.delta": {"output_index"},
	"response.reasoning_summary_text.delta":  {"output_index", "summary_index"},
}

// zeroIndexScenarios 是把上述事件类型全部逼出来所需的首块成色。
// 首块即序号为 0 的块，正是零值会被 omitempty 吃掉的那一格。
func zeroIndexScenarios() map[string][]ir.Event {
	return map[string][]ir.Event{
		"text-first": {
			{Type: ir.EvMessageStart, MessageID: "msg_1", Model: "native"},
			{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
			{Type: ir.EvTextDelta, Index: 0, Text: "hi"},
			{Type: ir.EvBlockStop, Index: 0},
			{Type: ir.EvMessageStop},
		},
		"thinking-first": {
			{Type: ir.EvMessageStart, MessageID: "msg_1", Model: "native"},
			{Type: ir.EvBlockStart, Index: 0,
				Block: &ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{}}},
			{Type: ir.EvThinkingDelta, Index: 0, Text: "pondering"},
			{Type: ir.EvBlockStop, Index: 0},
			{Type: ir.EvMessageStop},
		},
		"tool-first": {
			{Type: ir.EvMessageStart, MessageID: "msg_1", Model: "native"},
			{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockToolUse,
				ToolUse: &ir.ToolUse{ID: "call_1", Name: "grep"}}},
			{Type: ir.EvToolInput, Index: 0, Text: `{"pattern":"TODO"}`},
			{Type: ir.EvBlockStop, Index: 0},
			{Type: ir.EvMessageStop},
		},
	}
}

// TestZeroIndexFieldMatrix 是事件类型 × 必填字段的矩阵：首块的序号字段
// 必须以字面 0 出现，不能因为是零值就缺席。
func TestZeroIndexFieldMatrix(t *testing.T) {
	seen := map[string]bool{}
	for name, events := range zeroIndexScenarios() {
		rendered := renderStream(t, codec.ProtocolResponses, events)
		// 同一成色的非流式形态也过一遍稳定性辅助：序号字段在非流式响应里
		// 由数组下标表达，这里要的是两条编码路径仍逐字节一致。
		responseLossyOf(t, codec.ProtocolResponses, func() *ir.Response {
			return responseFromEvents(events)
		})
		for _, f := range scanFrames(t, rendered) {
			required, ok := zeroIndexFields[f.Event]
			if !ok {
				continue
			}
			seen[f.Event] = true
			for _, field := range required {
				t.Run(f.Event+"/"+field+"/"+name, func(t *testing.T) {
					var obj map[string]json.RawMessage
					if err := json.Unmarshal([]byte(f.Data), &obj); err != nil {
						t.Fatalf("unmarshal frame: %v\n%s", err, f.Data)
					}
					raw, ok := obj[field]
					if !ok {
						t.Fatalf("%s 缺 %s：零值被 omitempty 吃掉，消费方定位不到\n%s",
							f.Event, field, f.Data)
					}
					if string(raw) != "0" {
						t.Fatalf("%s 的 %s = %s，首块应为 0", f.Event, field, raw)
					}
				})
			}
		}
	}
	// 表里的事件类型必须都被上面的成色逼出来过，否则矩阵有格没跑。
	for kind := range zeroIndexFields {
		if !seen[kind] {
			t.Errorf("%s 未被任何成色触发，矩阵这一行是空跑", kind)
		}
	}
}

// responseFromEvents 把块生命周期事件折成等价的非流式响应，
// 让同一成色既能验流式帧、又能过响应侧的稳定性辅助。
func responseFromEvents(events []ir.Event) *ir.Response {
	resp := &ir.Response{Model: "native", StopReason: ir.StopEndTurn}
	byIndex := map[int]int{}
	for _, ev := range events {
		switch ev.Type {
		case ir.EvBlockStart:
			if ev.Block == nil {
				continue
			}
			// 深拷子结构：直接复制块会让 ToolUse / Thinking 指针与调用方的
			// 事件切片共用，往里追增量等于改事件本身，第二次调用就会累加两遍。
			block := *ev.Block
			if block.ToolUse != nil {
				tu := *block.ToolUse
				block.ToolUse = &tu
			}
			if block.Thinking != nil {
				th := *block.Thinking
				block.Thinking = &th
			}
			byIndex[ev.Index] = len(resp.Content)
			resp.Content = append(resp.Content, block)
		case ir.EvTextDelta:
			if i, ok := byIndex[ev.Index]; ok {
				resp.Content[i].Text += ev.Text
			}
		case ir.EvThinkingDelta:
			if i, ok := byIndex[ev.Index]; ok && resp.Content[i].Thinking != nil {
				resp.Content[i].Thinking.Text += ev.Text
			}
		case ir.EvToolInput:
			if i, ok := byIndex[ev.Index]; ok && resp.Content[i].ToolUse != nil {
				resp.Content[i].ToolUse.Input += ev.Text
			}
		}
	}
	return resp
}

// --- 9.3 响应侧签名同族矩阵 ---

// upstreamSignsThinking 回答该上游会不会给出推理签名。
// 取能力位而非写死清单：新出站协议进注册表即自动入矩阵。
func upstreamSignsThinking(t *testing.T, up string) bool {
	t.Helper()
	oc, ok := codec.Outbound(up)
	if !ok {
		t.Fatalf("outbound %q not registered", up)
	}
	return oc.Caps().ThinkingSig
}

// clientExpressesSignature 回答该客户端协议有没有承载签名的字段。
// 与上游侧取同一个能力位：同名协议两侧是同一套 wire 形态。
func clientExpressesSignature(t *testing.T, client string) bool {
	t.Helper()
	return upstreamSignsThinking(t, client)
}

// TestResponseSignatureMatrixIsFamilyScoped 是 4 上游 × 3 客户端的签名矩阵。
//
// 签名只在同族内有效：跨族透传的密文客户端验不了，还会被它存进历史，
// 下一轮带回来令整个请求被上游拒收。所以这里断言的是「同族且客户端
// 表达得了才保留，其余一律剥离并留下说明」。
func TestResponseSignatureMatrixIsFamilyScoped(t *testing.T) {
	const sig = "sig-upstream"
	for _, up := range outboundNames() {
		if !upstreamSignsThinking(t, up) {
			continue
		}
		for _, client := range inboundNames() {
			keep := up == client && clientExpressesSignature(t, client)

			t.Run("stream/"+up+"→"+client, func(t *testing.T) {
				events := []ir.Event{
					{Type: ir.EvMessageStart, MessageID: "msg_1", Model: "native"},
					{Type: ir.EvBlockStart, Index: 0,
						Block: &ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{}}},
					{Type: ir.EvThinkingDelta, Index: 0, Text: "pondering"},
					{Type: ir.EvSigDelta, Index: 0, Text: sig, SignatureFrom: up},
					{Type: ir.EvBlockStop, Index: 0},
					{Type: ir.EvMessageStop},
				}
				rendered, notes := renderStreamWithNotes(t, client, events)
				assertSignatureHandling(t, keep, sig, rendered, notes)
			})

			t.Run("non-stream/"+up+"→"+client, func(t *testing.T) {
				build := func() *ir.Response {
					return &ir.Response{
						Model:      "native",
						StopReason: ir.StopEndTurn,
						Content: []ir.Block{{Type: ir.BlockThinking, Thinking: &ir.Thinking{
							Text: "pondering", Signature: sig, SignatureFrom: up,
						}}},
					}
				}
				body, notes := responseLossyOf(t, client, build)
				assertSignatureHandling(t, keep, sig, string(body), notes)
			})
		}
	}
}

// assertSignatureHandling 是两条路径共用的判定：措辞与去留必须一致，
// 否则同一段推理经流式存下来、再以非流式回放就会被上游拒收。
func assertSignatureHandling(t *testing.T, keep bool, sig, encoded string, notes []string) {
	t.Helper()
	if keep {
		if !strings.Contains(encoded, sig) {
			t.Errorf("同族签名被丢掉了：\n%s", encoded)
		}
		if len(notes) != 0 {
			t.Errorf("同族签名不该有说明：%v", notes)
		}
		return
	}
	if strings.Contains(encoded, sig) {
		t.Errorf("签名泄漏进客户端内容：\n%s", encoded)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "thinking signature") {
		t.Errorf("剥离签名应恰报一条说明，实得 %v", notes)
	}
}

// --- 9.4 强制工具 × 推理矩阵 ---

// forcedToolModes 是两种「强制模型调工具」的表达。auto / none 不进矩阵：
// 它们是「模型自己决定」与「不要调」，与推理无冲突。
func forcedToolModes() []ir.ToolChoiceMode {
	return []ir.ToolChoiceMode{ir.ToolChoiceAny, ir.ToolChoiceTool}
}

// TestForcedToolsThinkingMatrix 是能力位两档 × 4 出站的矩阵。
//
// 冲突时关推理、留工具约束：降级工具约束的故障不可见——上游会正常回一段
// 文本，调用方以为模型选择了不调工具，而真相是约束被我们悄悄改掉了。
func TestForcedToolsThinkingMatrix(t *testing.T) {
	for _, out := range outboundNames() {
		oc, ok := codec.Outbound(out)
		if !ok {
			t.Fatalf("outbound %q not registered", out)
		}
		caps := oc.Caps()
		if !caps.Thinking {
			continue
		}
		for _, mode := range forcedToolModes() {
			t.Run(out+"/"+string(mode), func(t *testing.T) {
				req := probeRequest(ir.Block{Type: ir.BlockText, Text: "ok"})
				req.Thinking = &ir.ThinkingConfig{Enabled: ir.ThinkingOn(), Effort: "medium", BudgetTokens: 4096}
				req.Tools = []ir.Tool{{Name: "grep", Description: "search",
					Schema: `{"type":"object","properties":{"pattern":{"type":"string"}}}`}}
				req.ToolChoice = &ir.ToolChoice{Mode: mode, Name: "grep"}

				body, notes := lossyOf(t, out, req)
				// 工具约束在两档下都必须原样抵达上游。
				if !hasForcedToolChoice(t, out, body) {
					t.Fatalf("%s 丢了强制工具约束：%s", out, body)
				}
				thinking := hasThinkingRequest(t, out, body)

				if !caps.ThinkingExcludesForcedTools {
					if !thinking {
						t.Errorf("%s 未声明互斥，推理不该被关掉：%s", out, body)
					}
					if hasForcedToolsNote(notes) {
						t.Errorf("%s 未声明互斥，不该报取舍说明：%v", out, notes)
					}
					return
				}
				if thinking {
					t.Errorf("%s 声明互斥，推理应被关掉：%s", out, body)
				}
				if !hasForcedToolsNote(notes) {
					t.Errorf("%s 关掉推理必须留说明，实得 %v", out, notes)
				}
			})
		}
	}
}

func hasForcedToolsNote(notes []string) bool {
	for _, n := range notes {
		if strings.Contains(n, "forced tool choice") {
			return true
		}
	}
	return false
}

// hasForcedToolChoice 按各协议的表达方式检查强制约束是否抵达 wire body。
func hasForcedToolChoice(t *testing.T, protocol string, body []byte) bool {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("%s: unmarshal body: %v", protocol, err)
	}
	if protocol == codec.ProtocolGemini {
		// 本协议用 toolConfig.functionCallingConfig.mode=ANY 表达强制，
		// 「必须用某一个工具」再加 allowedFunctionNames 白名单。
		var cfg struct {
			FunctionCallingConfig struct {
				Mode string `json:"mode"`
			} `json:"functionCallingConfig"`
		}
		if err := json.Unmarshal(m["toolConfig"], &cfg); err != nil {
			return false
		}
		return cfg.FunctionCallingConfig.Mode == "ANY"
	}
	raw, ok := m["tool_choice"]
	if !ok {
		return false
	}
	// 三个协议的强制形态各异：anthropic 是 {"type":"any"|"tool"}，
	// 另两个是字符串 "required" 或一个具名对象。共同点是不为 auto / none。
	s := strings.Trim(string(raw), `"`)
	return s != "auto" && s != "none"
}

// --- 9.5 同 id 跨 index 矩阵 ---

// sameIDAcrossIndexStreams 是「同一次工具调用的分片带着不同 index」的上游流。
//
// 只有 chat_completions 有这一格：另外三个协议的块身份不靠 index 与 id
// 两套并行的标识表达，见 protocolLimitations 的登记。
var sameIDAcrossIndexStreams = map[string]string{
	codec.ProtocolChatCompletions: strings.Join([]string{
		`data: {"id":"msg_1","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"dup","type":"function","function":{"name":"grep","arguments":"{\"pattern\":"}}]}}]}`,
		``,
		`data: {"id":"msg_1","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"dup","function":{"arguments":"\"TODO\"}"}}]}}]}`,
		``,
		`data: {"id":"msg_1","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"),
}

// TestSameIDAcrossIndexMatrix 断言同 id 的分片并回一个块。
//
// 不并的后果是客户端收到两个 tool_use 块、拿着两份半截入参各执行一次，
// 而 HTTP 状态码是 200——流水里看不出任何异常。
func TestSameIDAcrossIndexMatrix(t *testing.T) {
	for _, up := range outboundNames() {
		raw, ok := sameIDAcrossIndexStreams[up]
		if !ok {
			continue
		}
		t.Run(up, func(t *testing.T) {
			resp, notes := decodeStreamWithNotes(t, up, raw)
			calls := toolUses(resp)
			if len(calls) != 1 {
				t.Fatalf("同 id 分片解出 %d 个工具块，应合并成 1 个", len(calls))
			}
			if got := strings.TrimSpace(calls[0].Input); got != `{"pattern":"TODO"}` {
				t.Errorf("合并后的入参 = %s", got)
			}
			if !hasNoteContaining(notes, "different indexes") {
				t.Errorf("合并是我们的判断，必须留说明，实得 %v", notes)
			}
			// 合并出的块还得能原样编给客户端，且两条编码路径逐字节一致。
			responseLossyOf(t, codec.ProtocolAnthropic, func() *ir.Response { return resp })
		})
	}
}

// --- 9.6 多文档行拆分矩阵 ---

// multiDocLines 是各上游「一行里挤了两个 JSON 文档」的形态，
// 两份都是该协议的合法帧，合起来的文本内容为 "ab"。
var multiDocLines = map[string][2]string{
	codec.ProtocolAnthropic: {
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"b"}}`,
	},
	codec.ProtocolChatCompletions: {
		`{"id":"msg_1","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{"content":"a"}}]}`,
		`{"id":"msg_1","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{"content":"b"}}]}`,
	},
	codec.ProtocolResponses: {
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"a"}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"b"}`,
	},
	codec.ProtocolGemini: {
		`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"a"}]}}]}`,
		`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"b"}]}}]}`,
	},
}

// TestMultiDocLineMatrix 是畸形形态 × 4 上游的矩阵。
//
// 两种形态分开断言：两份都合法时必须都解出来并留说明（丢一份等于内容
// 无声缺失）；后半截残缺时必须整行失败（部分解码会让客户端收到半截内容，
// 却看不出后面丢了东西）。
func TestMultiDocLineMatrix(t *testing.T) {
	for _, up := range outboundNames() {
		pair, ok := multiDocLines[up]
		if !ok {
			t.Errorf("%s 缺多文档行 fixture", up)
			continue
		}

		t.Run("two-valid-docs/"+up, func(t *testing.T) {
			resp, notes := feedLineWithNotes(t, up, pair[0]+pair[1])
			if got := responseText(resp); got != "ab" {
				t.Errorf("拆分后文本 = %q，应为 ab", got)
			}
			if !hasNoteContaining(notes, "several JSON documents") {
				t.Errorf("拆分必须留说明，实得 %v", notes)
			}
			responseLossyOf(t, codec.ProtocolAnthropic, func() *ir.Response { return resp })
		})

		t.Run("truncated-second-doc/"+up, func(t *testing.T) {
			dec := newDecoder(t, up)
			if _, err := dec.Feed("", pair[0]+`{"type":`); err == nil {
				t.Fatal("后半截残缺应整行失败，不能部分解码")
			}
			if notes := decoderNotes(dec); hasNoteContaining(notes, "several JSON documents") {
				t.Errorf("整行失败不该报拆分成功：%v", notes)
			}
		})
	}
}

// --- 共用辅助 ---

func scanFrames(t *testing.T, raw string) []codec.Frame {
	t.Helper()
	sc := codec.NewFrameScanner(strings.NewReader(raw))
	var out []codec.Frame
	for sc.Scan() {
		out = append(out, sc.Frame())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return out
}

// renderStreamWithNotes 编流并取编码器说明。
func renderStreamWithNotes(t *testing.T, protocol string, events []ir.Event) (string, []string) {
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
	n, ok := enc.(codec.StreamNotes)
	if !ok {
		t.Fatalf("%s stream encoder does not report notes", protocol)
	}
	return b.String(), n.Notes()
}

func newDecoder(t *testing.T, protocol string) codec.StreamDecoder {
	t.Helper()
	c, ok := codec.Outbound(protocol)
	if !ok {
		t.Fatalf("outbound %q not registered", protocol)
	}
	return c.NewStreamDecoder()
}

func decoderNotes(dec codec.StreamDecoder) []string {
	n, ok := dec.(codec.StreamNotes)
	if !ok {
		return nil
	}
	return n.Notes()
}

// decodeStreamWithNotes 解一段上游流，返回聚合结果与解码器说明。
func decodeStreamWithNotes(t *testing.T, protocol, raw string) (*ir.Response, []string) {
	t.Helper()
	dec := newDecoder(t, protocol)
	var agg ir.Aggregator
	sc := codec.NewFrameScanner(strings.NewReader(raw))
	for sc.Scan() {
		events, err := dec.Feed(sc.Frame().Event, sc.Frame().Data)
		if err != nil {
			t.Fatalf("%s feed %q: %v", protocol, sc.Frame().Data, err)
		}
		for _, ev := range events {
			agg.Add(ev)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("%s scan: %v", protocol, err)
	}
	for _, ev := range dec.Finish() {
		agg.Add(ev)
	}
	return agg.Response(), decoderNotes(dec)
}

// feedLineWithNotes 把一整行直接喂给解码器，绕开 SSE 切帧：
// 多文档行的关键正是「一行里有多份」，切帧器不会替我们分开。
func feedLineWithNotes(t *testing.T, protocol, line string) (*ir.Response, []string) {
	t.Helper()
	dec := newDecoder(t, protocol)
	var agg ir.Aggregator
	events, err := dec.Feed("", line)
	if err != nil {
		t.Fatalf("%s feed: %v", protocol, err)
	}
	for _, ev := range events {
		agg.Add(ev)
	}
	for _, ev := range dec.Finish() {
		agg.Add(ev)
	}
	return agg.Response(), decoderNotes(dec)
}

func toolUses(resp *ir.Response) []*ir.ToolUse {
	var out []*ir.ToolUse
	for i := range resp.Content {
		if resp.Content[i].Type == ir.BlockToolUse && resp.Content[i].ToolUse != nil {
			out = append(out, resp.Content[i].ToolUse)
		}
	}
	return out
}

func hasNoteContaining(notes []string, want string) bool {
	for _, n := range notes {
		if strings.Contains(n, want) {
			return true
		}
	}
	return false
}
