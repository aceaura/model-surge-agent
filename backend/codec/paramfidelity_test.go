package codec_test

import (
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

// 这个文件守的是调参字段的保真。收益首先在**同协议往返**：本服务不设透传
// 快捷路径，所有请求都过 IR，IR 没建模的字段在 chat_completions→
// chat_completions 这种同协议路径上同样消失——客户端换用本服务后，一个
// 它一直在用的字段无声失效了。跨协议是顺带覆盖的一层。

// paramFixture 是各入站协议「把自己支持的调参字段全给齐」的请求。
var paramFixture = map[string]string{
	codec.ProtocolChatCompletions: `{
	  "model":"user-model","max_tokens":256,
	  "messages":[{"role":"user","content":"hi"}],
	  "presence_penalty":0.5,"frequency_penalty":-0.25,
	  "seed":42,"n":3,
	  "logprobs":true,"top_logprobs":5,
	  "logit_bias":{"123":1.5},
	  "service_tier":"priority",
	  "parallel_tool_calls":false,
	  "response_format":{"type":"json_object"}
	}`,
	codec.ProtocolResponses: `{
	  "model":"user-model","max_output_tokens":256,"input":"hi",
	  "include":["reasoning.encrypted_content"],
	  "truncation":"auto",
	  "metadata":{"trace":"abc"},
	  "service_tier":"priority",
	  "parallel_tool_calls":false,
	  "top_logprobs":5,
	  "text":{"verbosity":"low","format":{"type":"json_object"}}
	}`,
}

func decodeParams(t *testing.T, proto string) *ir.Request {
	t.Helper()
	in, ok := codec.Inbound(proto)
	if !ok {
		t.Fatalf("no inbound codec for %s", proto)
	}
	req, err := in.DecodeRequest([]byte(paramFixture[proto]))
	if err != nil {
		t.Fatalf("%s decode: %v", proto, err)
	}
	return req
}

// TestChatCompletionsParamsDecodeIntoIR 钉住入站解码：这批字段此前在
// wire 结构体里不存在，被 encoding/json 静默吞掉。
func TestChatCompletionsParamsDecodeIntoIR(t *testing.T) {
	req := decodeParams(t, codec.ProtocolChatCompletions)
	if req.PresencePenalty == nil || *req.PresencePenalty != 0.5 {
		t.Errorf("presence_penalty = %v", req.PresencePenalty)
	}
	if req.FrequencyPenalty == nil || *req.FrequencyPenalty != -0.25 {
		t.Errorf("frequency_penalty = %v", req.FrequencyPenalty)
	}
	if req.Seed == nil || *req.Seed != 42 {
		t.Errorf("seed = %v", req.Seed)
	}
	if req.Candidates == nil || *req.Candidates != 3 {
		t.Errorf("n = %v", req.Candidates)
	}
	if req.LogProbs == nil || !*req.LogProbs {
		t.Errorf("logprobs = %v", req.LogProbs)
	}
	if req.TopLogProbs == nil || *req.TopLogProbs != 5 {
		t.Errorf("top_logprobs = %v", req.TopLogProbs)
	}
	if req.LogitBias["123"] != 1.5 {
		t.Errorf("logit_bias = %v", req.LogitBias)
	}
	if req.ServiceTier != "priority" {
		t.Errorf("service_tier = %q", req.ServiceTier)
	}
	// 三态：明确 false 必须与「没给」分开，否则出站会替客户端表态。
	if req.ParallelToolCalls == nil || *req.ParallelToolCalls {
		t.Errorf("parallel_tool_calls = %v, want explicit false", req.ParallelToolCalls)
	}
	if req.ResponseFormat == nil || req.ResponseFormat.Kind != ir.ResponseFormatJSON {
		t.Errorf("response_format = %+v", req.ResponseFormat)
	}
}

func TestResponsesParamsDecodeIntoIR(t *testing.T) {
	req := decodeParams(t, codec.ProtocolResponses)
	if len(req.Include) != 1 || req.Include[0] != "reasoning.encrypted_content" {
		t.Errorf("include = %v", req.Include)
	}
	if req.Truncation != "auto" {
		t.Errorf("truncation = %q", req.Truncation)
	}
	if req.ClientMetadata["trace"] != "abc" {
		t.Errorf("metadata = %v", req.ClientMetadata)
	}
	if req.Verbosity != "low" {
		t.Errorf("verbosity = %q", req.Verbosity)
	}
	if req.ResponseFormat == nil || req.ResponseFormat.Kind != ir.ResponseFormatJSON {
		t.Errorf("text.format = %+v", req.ResponseFormat)
	}
	// ClientMetadata 与 Metadata 必须分开：后者只承载 user_id 且被翻译成
	// 各协议的用户标识字段，混用会让 user_id 作为普通元数据再发一遍。
	if _, has := req.Metadata["trace"]; has {
		t.Errorf("客户端元数据漏进了 Metadata: %v", req.Metadata)
	}
}

// TestParamsSurviveSameProtocolRoundTrip 是本轮的核心断言：同协议往返
// 一个字段都不能丢。
func TestParamsSurviveSameProtocolRoundTrip(t *testing.T) {
	cases := map[string][]string{
		codec.ProtocolChatCompletions: {
			`"presence_penalty":0.5`, `"frequency_penalty":-0.25`,
			`"seed":42`, `"n":3`, `"logprobs":true`, `"top_logprobs":5`,
			`"logit_bias":{"123":1.5}`, `"service_tier":"priority"`,
			`"parallel_tool_calls":false`, `"response_format":{"type":"json_object"}`,
		},
		codec.ProtocolResponses: {
			`"include":["reasoning.encrypted_content"]`, `"truncation":"auto"`,
			`"metadata":{"trace":"abc"}`, `"service_tier":"priority"`,
			`"parallel_tool_calls":false`, `"top_logprobs":5`,
			`"verbosity":"low"`, `"type":"json_object"`,
		},
	}
	for proto, wants := range cases {
		t.Run(proto, func(t *testing.T) {
			req := decodeParams(t, proto)
			body := string(encodeOut(t, proto, req))
			for _, w := range wants {
				if !strings.Contains(body, w) {
					t.Errorf("同协议往返丢了 %s: %s", w, body)
				}
			}
			// 只断言没有 dropped：同协议往返不该丢任何字段。
			// forwarded-but-unreturned 那一类是允许的——logprobs 在
			// 同协议往返上照样发给了上游，只是结果回不来（第十六轮）。
			for _, n := range paramNotes(t, proto, req) {
				if strings.HasPrefix(n, "dropped ") {
					t.Errorf("同协议往返不该丢字段: %s", n)
				}
			}
		})
	}
}

// paramNotes 取某出站协议对这份请求的有损诊断。走既有的 lossyOf：
// 它同时断言 EncodeRequest 与 EncodeRequestLossy 的请求体逐字节相同。
func paramNotes(t *testing.T, proto string, req *ir.Request) []string {
	t.Helper()
	_, notes := lossyOf(t, proto, req)
	return notes
}

// TestUnsupportedParamsAreReportedLossy 是跨协议那一层：目标协议表达不了
// 就必须报有损。静默丢弃是本轮要修的原始故障。
func TestUnsupportedParamsAreReportedLossy(t *testing.T) {
	cases := []struct {
		field string
		set   func(*ir.Request)
	}{
		{"presence_penalty", func(r *ir.Request) { v := 0.5; r.PresencePenalty = &v }},
		{"frequency_penalty", func(r *ir.Request) { v := 0.5; r.FrequencyPenalty = &v }},
		{"seed", func(r *ir.Request) { v := 7; r.Seed = &v }},
		{"n", func(r *ir.Request) { v := 3; r.Candidates = &v }},
		{"logprobs", func(r *ir.Request) { v := true; r.LogProbs = &v }},
		{"logit_bias", func(r *ir.Request) { r.LogitBias = map[string]float64{"1": 1} }},
		{"service_tier", func(r *ir.Request) { r.ServiceTier = "priority" }},
		{"parallel_tool_calls", func(r *ir.Request) { v := true; r.ParallelToolCalls = &v }},
		{"response_format", func(r *ir.Request) { r.ResponseFormat = &ir.ResponseFormat{Kind: ir.ResponseFormatJSON} }},
		{"verbosity", func(r *ir.Request) { r.Verbosity = "low" }},
		{"include", func(r *ir.Request) { r.Include = []string{"x"} }},
		{"truncation", func(r *ir.Request) { r.Truncation = "auto" }},
		{"metadata", func(r *ir.Request) { r.ClientMetadata = map[string]string{"k": "v"} }},
		{"max_tool_calls", func(r *ir.Request) { n := 3; r.MaxToolCalls = &n }},
		{"stream_options.include_obfuscation", func(r *ir.Request) {
			v := true
			r.IncludeObfuscation = &v
		}},
	}
	for _, c := range cases {
		for _, out := range outboundNames() {
			t.Run(c.field+"→"+out, func(t *testing.T) {
				req := paramBase()
				c.set(req)
				notes := paramNotes(t, out, req)
				// 只看 dropped 那一类：目标支持某字段时仍可能有
				// forwarded-but-unreturned 的说明（logprobs 就是），
				// 那不是「表达不了」，混在一起判会把两件事搅成一件。
				reported := containsDroppedField(notes, c.field)
				supported := supportsField(t, out, c.field)
				if supported && reported {
					t.Errorf("%s 支持 %s，不该报丢弃: %v", out, c.field, notes)
				}
				if !supported && !reported {
					t.Errorf("%s 表达不了 %s，必须报丢弃: %v", out, c.field, notes)
				}
			})
		}
	}
}

func paramBase() *ir.Request {
	return &ir.Request{
		Model:     "m",
		MaxTokens: 256,
		Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}
}

// supportsField 从能力位读某出站协议是否承载该字段。从 Caps 读而不是
// 写死一张表：新增出站协议时这里自动跟上，写死的表会静默过期。
func supportsField(t *testing.T, proto, field string) bool {
	t.Helper()
	oc, _ := codec.Outbound(proto)
	caps := oc.Caps()
	switch field {
	case "presence_penalty", "frequency_penalty":
		return caps.Penalties
	case "seed":
		return caps.Seed
	case "n":
		return caps.Candidates
	case "logprobs", "top_logprobs":
		return caps.LogProbs
	case "logit_bias":
		return caps.LogitBias
	case "service_tier":
		return caps.ServiceTier
	case "parallel_tool_calls":
		return caps.ParallelToolCalls
	case "response_format":
		return caps.ResponseFormat
	case "verbosity":
		return caps.Verbosity
	case "include":
		return caps.Include
	case "truncation":
		return caps.Truncation
	case "metadata":
		return caps.ClientMetadata
	case "max_tool_calls":
		return caps.MaxToolCalls
	case "stream_options.include_obfuscation":
		return caps.StreamObfuscation
	default:
		t.Fatalf("未知字段 %s——新增调参字段必须在这里给出能力位映射", field)
		return false
	}
}

func containsField(notes []string, field string) bool {
	for _, n := range notes {
		// 带前后空格匹配，避免 logprobs 命中 top_logprobs、
		// metadata 命中 client_metadata 这类子串误判。
		if strings.Contains(n, " "+field+" ") {
			return true
		}
	}
	return false
}

// containsDroppedField 只认 dropped 那一类说明。
func containsDroppedField(notes []string, field string) bool {
	for _, n := range notes {
		if strings.HasPrefix(n, "dropped "+field+" ") {
			return true
		}
	}
	return false
}

// TestAbsentParamsAreNotReportedLossy 是对照：客户端没给的字段一条都不报。
// 承第十四轮判据——没提不等于被丢弃。报了会让每个普通请求都拖着一串
// 无意义的说明，真正的丢弃反而看不见。
func TestAbsentParamsAreNotReportedLossy(t *testing.T) {
	for _, out := range outboundNames() {
		t.Run(out, func(t *testing.T) {
			if notes := paramNotes(t, out, paramBase()); len(notes) != 0 {
				t.Errorf("什么都没给却报了有损: %v", notes)
			}
		})
	}
}

// TestSchemaDowngradesToPlainJSON：目标支持 JSON 但不支持 schema 时，
// 要求降级成「只要求是 JSON」而不是整个丢掉——客户端的最低要求仍满足。
func TestSchemaDowngradeKeepsJSONRequirement(t *testing.T) {
	const schema = `{"type":"object","properties":{"a":{"type":"string"}}}`
	for _, out := range outboundNames() {
		t.Run(out, func(t *testing.T) {
			oc, _ := codec.Outbound(out)
			caps := oc.Caps()
			req := paramBase()
			req.ResponseFormat = &ir.ResponseFormat{Kind: ir.ResponseFormatSchema, Name: "r", Schema: schema}
			bodyStr, notes, err := mustEncode(t, out, req)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			body := []byte(bodyStr)
			switch {
			case caps.ResponseSchema:
				// 支持 schema：约束必须真的出现在请求体里，只写「要 JSON」
				// 等于悄悄放弃了结构约束。anthropic 走 output_config.format
				// 也在这一档。
				if !strings.Contains(string(body), `"a"`) {
					t.Errorf("声称支持 schema 但请求体里没有约束: %s", body)
				}
			case caps.ResponseFormat:
				// 支持 JSON 但不支持 schema：降级成纯 JSON 模式，
				// 客户端的最低要求仍满足。
				if !strings.Contains(string(body), "json") {
					t.Errorf("支持 JSON 就必须保住 JSON 要求: %s", body)
				}
			default:
				// 两档都表达不了：必须报有损。
				if !containsField(notes, "response_format") {
					t.Errorf("表达不了结构化输出必须报有损: %v", notes)
				}
			}
		})
	}
}

// TestGeminiSchemaGoesThroughDialect：gemini 的 responseSchema 与工具 schema
// 受同一个 OpenAPI 3.0 子集约束，原样发完整 JSON Schema 会让整轮 400。
func TestGeminiSchemaGoesThroughDialect(t *testing.T) {
	req := paramBase()
	req.ResponseFormat = &ir.ResponseFormat{
		Kind:   ir.ResponseFormatSchema,
		Schema: `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","additionalProperties":false,"properties":{"a":{"type":"string"}}}`,
	}
	body := string(encodeOut(t, codec.ProtocolGemini, req))
	for _, banned := range []string{"$schema", "additionalProperties"} {
		if strings.Contains(body, banned) {
			t.Errorf("方言外的关键字 %s 原样发出会被上游拒收: %s", banned, body)
		}
	}
	if !strings.Contains(body, `"responseMimeType":"application/json"`) {
		t.Errorf("给了 schema 就必须同时给 mimeType: %s", body)
	}
	if !strings.Contains(body, "OBJECT") {
		t.Errorf("type 必须大写化: %s", body)
	}
}

// TestChatSchemaSurvivesThroughAnthropic：chat 入站的 schema 经 anthropic 出站
// 再回解，约束不丢。anthropic 的 output_config.format 只有 json_schema 形态，
// 恰好接得住 chat 的 schema 档（name/strict 没有槽位，丢掉是预期）。
func TestChatSchemaSurvivesThroughAnthropic(t *testing.T) {
	const schema = `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"response_format":{"type":"json_schema","json_schema":{"name":"weather","strict":true,"schema":` + schema + `}}}`)
	ic, ok := codec.Inbound(codec.ProtocolChatCompletions)
	if !ok {
		t.Fatal("inbound chat_completions not registered")
	}
	req, err := ic.DecodeRequest(body)
	if err != nil {
		t.Fatalf("chat DecodeRequest: %v", err)
	}
	req.MaxTokens = 100
	oc, ok := codec.Outbound(codec.ProtocolAnthropic)
	if !ok {
		t.Fatal("outbound anthropic not registered")
	}
	out, err := oc.EncodeRequest(req)
	if err != nil {
		t.Fatalf("anthropic EncodeRequest: %v", err)
	}
	if !strings.Contains(string(out), `"city"`) {
		t.Fatalf("schema 本体没过去：%s", out)
	}
	aic, ok := codec.Inbound(codec.ProtocolAnthropic)
	if !ok {
		t.Fatal("inbound anthropic not registered")
	}
	back, err := aic.DecodeRequest(out)
	if err != nil {
		t.Fatalf("回解: %v", err)
	}
	if back.ResponseFormat == nil || back.ResponseFormat.Kind != ir.ResponseFormatSchema {
		t.Fatalf("往返后约束丢了：%+v", back.ResponseFormat)
	}
	if back.ResponseFormat.Schema != schema {
		t.Errorf("往返后 schema 变了：%q", back.ResponseFormat.Schema)
	}
}

// TestAnthropicStructuredOutputIsSchemaOnly：anthropic 的 output_config.format
// 只接 json_schema。schema 档原样送达、诊断闭嘴；纯 JSON 模式没有落点，
// 必须报「只接 schema 约束形态」的受限措辞，而不是笼统的「无结构化输出」。
func TestAnthropicStructuredOutputIsSchemaOnly(t *testing.T) {
	const schema = `{"type":"object","properties":{"a":{"type":"string"}}}`

	schemaReq := paramBase()
	schemaReq.ResponseFormat = &ir.ResponseFormat{Kind: ir.ResponseFormatSchema, Schema: schema}
	for _, n := range paramNotes(t, codec.ProtocolAnthropic, schemaReq) {
		if strings.Contains(n, "response_format") {
			t.Errorf("anthropic 接得住 schema 档，误报有损：%s", n)
		}
	}

	jsonReq := paramBase()
	jsonReq.ResponseFormat = &ir.ResponseFormat{Kind: ir.ResponseFormatJSON}
	notes := paramNotes(t, codec.ProtocolAnthropic, jsonReq)
	if !containsDroppedField(notes, "response_format") {
		t.Fatalf("anthropic 纯 JSON 模式必须报丢弃：%v", notes)
	}
	if !strings.Contains(strings.Join(notes, "; "), "only schema-constrained structured output") {
		t.Errorf("anthropic 纯 JSON 模式未报受限措辞：%v", notes)
	}
}

// 不生效，只给了 top_logprobs 时必须补上开关，否则要求被丢掉。
func TestGeminiLogprobsSwitchIsFilledIn(t *testing.T) {
	req := paramBase()
	n := 5
	req.TopLogProbs = &n
	body := string(encodeOut(t, codec.ProtocolGemini, req))
	if !strings.Contains(body, `"responseLogprobs":true`) {
		t.Errorf("给了 logprobs 数量却没开开关，要求会被上游忽略: %s", body)
	}
}

// TestMaxTokensIsPassedThroughVerbatim 是需求 3 的第一条：客户端给了就原样发。
// 特别包含一个极小值——sub2api 会把它静默抬到 128，那是无痕改写客户端意图。
func TestMaxTokensIsPassedThroughVerbatim(t *testing.T) {
	for _, out := range outboundNames() {
		for _, want := range []int{10, 256} {
			t.Run(out, func(t *testing.T) {
				req := paramBase()
				req.MaxTokens = want
				body, notes, err := mustEncode(t, out, req)
				if err != nil {
					t.Fatalf("encode: %v", err)
				}
				if !strings.Contains(body, jsonNum(want)) {
					t.Errorf("客户端给的上限 %d 没原样发出: %s", want, body)
				}
				if containsField(notes, "max_tokens") {
					t.Errorf("客户端给了上限却报了兜底: %v", notes)
				}
			})
		}
	}
}

func jsonNum(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func mustEncode(t *testing.T, proto string, req *ir.Request) (string, []string, error) {
	t.Helper()
	oc, ok := codec.Outbound(proto)
	if !ok {
		t.Fatalf("no outbound codec for %s", proto)
	}
	le, ok := oc.(codec.LossyEncoder)
	if !ok {
		t.Fatalf("outbound %q must implement LossyEncoder", proto)
	}
	body, notes, err := le.EncodeRequestLossy(req.Clone())
	return string(body), notes, err
}

// TestMaxTokensAbsent 是需求 3 的另两格：可省则省、必填则兜底且留痕。
func TestMaxTokensAbsent(t *testing.T) {
	for _, out := range outboundNames() {
		t.Run(out, func(t *testing.T) {
			oc, _ := codec.Outbound(out)
			caps := oc.Caps()
			req := paramBase()
			req.MaxTokens = 0
			body, notes, err := mustEncode(t, out, req)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if !caps.RequiresMaxTokens {
				if strings.Contains(body, "max_tokens") || strings.Contains(body, "maxOutputTokens") ||
					strings.Contains(body, "max_output_tokens") {
					t.Errorf("客户端没给且本协议可省，不该写出上限: %s", body)
				}
				if containsField(notes, "max_tokens") {
					t.Errorf("没兜底却报了兜底: %v", notes)
				}
				return
			}
			if caps.DefaultMaxTokens <= 0 {
				t.Fatalf("%s 声明 max_tokens 必填却没有兜底值，编码本该失败", out)
			}
			if !strings.Contains(body, jsonNum(caps.DefaultMaxTokens)) {
				t.Errorf("必填协议应填入兜底值 %d: %s", caps.DefaultMaxTokens, body)
			}
			// 留痕是这一条的重点：不留痕的话长回答会在一个客户端从未设过的
			// 上限处被截断，而流水里毫无线索。
			if !containsField(notes, "max_tokens") {
				t.Errorf("兜底必须留痕: %v", notes)
			}
		})
	}
}

// TestParallelToolCallsThreeState：没给就不写，给了 false 要写 false。
// 坍缩成两态会替客户端表态——同第十四轮推理开关的判据。
func TestParallelToolCallsThreeState(t *testing.T) {
	for _, out := range outboundNames() {
		oc, _ := codec.Outbound(out)
		if !oc.Caps().ParallelToolCalls {
			continue
		}
		t.Run(out+"/absent", func(t *testing.T) {
			body := string(encodeOut(t, out, paramBase()))
			if strings.Contains(body, "parallel_tool_calls") {
				t.Errorf("客户端没给却写出了 parallel_tool_calls: %s", body)
			}
		})
		t.Run(out+"/explicit false", func(t *testing.T) {
			req := paramBase()
			v := false
			req.ParallelToolCalls = &v
			body := string(encodeOut(t, out, req))
			if !strings.Contains(body, `"parallel_tool_calls":false`) {
				t.Errorf("明确 false 必须写出: %s", body)
			}
		})
	}
}

// TestCloneCopiesParams 守的是换目标重试：Clone 漏拷一个字段，第二次
// 尝试就会与第一次发出不同的请求，而故障只在重试路径上出现。
func TestCloneCopiesParams(t *testing.T) {
	req := decodeParams(t, codec.ProtocolChatCompletions)
	req.Include = []string{"x"}
	req.ClientMetadata = map[string]string{"k": "v"}
	req.Verbosity = "low"
	req.Truncation = "auto"
	clone := req.Clone()

	a, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	b, err := json.Marshal(clone)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(a) != string(b) {
		t.Errorf("Clone 丢了字段:\n原件 %s\n副本 %s", a, b)
	}

	// 深拷贝：改副本不能影响原件，否则一次重试会污染后续尝试。
	*clone.Seed = 999
	clone.LogitBias["123"] = 999
	clone.ClientMetadata["k"] = "changed"
	clone.ResponseFormat.Kind = ir.ResponseFormatSchema
	if *req.Seed == 999 || req.LogitBias["123"] == 999 ||
		req.ClientMetadata["k"] == "changed" || req.ResponseFormat.Kind == ir.ResponseFormatSchema {
		t.Error("Clone 是浅拷贝，改副本影响了原件")
	}
}

// TestTextFormatTypeTextIsNotARequirement：type 为 text 是默认形态，
// 不是一项要求。解成非 nil 会让出站把「没要求」写成「要求纯文本」，
// 在不支持该字段的协议上还会多报一条假的有损诊断。
func TestPlainTextFormatIsNotARequirement(t *testing.T) {
	cases := map[string]string{
		codec.ProtocolChatCompletions: `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"response_format":{"type":"text"}}`,
		codec.ProtocolResponses:       `{"model":"m","max_output_tokens":16,"input":"hi","text":{"format":{"type":"text"}}}`,
	}
	for proto, body := range cases {
		t.Run(proto, func(t *testing.T) {
			in, _ := codec.Inbound(proto)
			req, err := in.DecodeRequest([]byte(body))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if req.ResponseFormat != nil {
				t.Errorf("type=text 不该解成一项要求: %+v", req.ResponseFormat)
			}
			if notes := paramNotes(t, codec.ProtocolAnthropic, req); containsField(notes, "response_format") {
				t.Errorf("没要求结构化却报了丢弃: %v", notes)
			}
		})
	}
}

// TestMaxTokensForCoversEveryCase 直接测判定函数。
//
// 「必填协议缺兜底值」这一格在四个现役协议上不可达（唯一必填的填了值），
// 只能这样测：不测它等于把一段保护代码交给未来的某个人去发现它从没生效。
func TestMaxTokensForCoversEveryCase(t *testing.T) {
	cases := []struct {
		name    string
		want    int
		caps    codec.Capabilities
		wantN   int
		wantOK  bool
		wantErr bool
	}{
		{"client gave a value", 256, codec.Capabilities{}, 256, true, false},
		{"client gave a tiny value", 10, codec.Capabilities{RequiresMaxTokens: true, DefaultMaxTokens: 4096}, 10, true, false},
		{"absent and optional", 0, codec.Capabilities{}, 0, false, false},
		{"absent and required with default", 0, codec.Capabilities{RequiresMaxTokens: true, DefaultMaxTokens: 8192}, 8192, true, false},
		{"absent and required without default", 0, codec.Capabilities{RequiresMaxTokens: true}, 0, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n, ok, err := codec.MaxTokensFor(c.want, c.caps)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if ok != c.wantOK {
				t.Errorf("ok = %v, want %v", ok, c.wantOK)
			}
			if n != c.wantN {
				t.Errorf("n = %d, want %d", n, c.wantN)
			}
		})
	}
}

// TestGeminiKeepsCleanSchemaVerbatim：方言内的 schema 一个字节都不该动。
// 与 TestGeminiSchemaGoesThroughDialect 是两格——那条测的是「脏 schema 被
// 归一」，走的是 Changed 分支；这条测的是「干净 schema 原样带上」，走的是
// 另一个分支。只测前者时后者可以整段删掉而测试仍绿。
func TestGeminiKeepsCleanSchemaVerbatim(t *testing.T) {
	req := paramBase()
	req.ResponseFormat = &ir.ResponseFormat{
		Kind:   ir.ResponseFormatSchema,
		Schema: `{"type":"OBJECT","properties":{"a":{"type":"STRING"}}}`,
	}
	body := string(encodeOut(t, codec.ProtocolGemini, req))
	if !strings.Contains(body, `"responseSchema"`) {
		t.Errorf("干净的 schema 也必须写出约束: %s", body)
	}
	if !strings.Contains(body, `"a"`) {
		t.Errorf("约束内容丢了: %s", body)
	}
}
