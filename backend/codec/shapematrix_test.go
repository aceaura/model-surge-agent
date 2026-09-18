package codec_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 这个文件是请求侧结构约束的矩阵：约束 × 出站协议。
//
// 与 crossmatrix_test.go 的分工是「请求结构」对「内容语义」：那边测四类语义
// 能否穿过 IR 抵达对面，这边测请求的形状是否被改成目标协议收得下的样子。
// 两边共用 outboundNames() 与 lossyOf()，后者顺带保证两条编码路径字节一致。
//
// 出站协议列表一律从注册表取：新增协议时这些矩阵会自动带上它。

// shapeRequest 是矩阵的基底请求。
//
// max_tokens 取 8192：anthropic 要求推理预算同时不低于 1024 且小于
// max_tokens，太小会让 shapeParams 关掉推理，测的就不是本矩阵的约束了。
func shapeRequest() *ir.Request {
	return &ir.Request{
		Model:     "native",
		MaxTokens: 8192,
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
		},
	}
}

func capsOf(t *testing.T, out string) codec.Capabilities {
	t.Helper()
	oc, ok := codec.Outbound(out)
	if !ok {
		t.Fatalf("outbound %q not registered", out)
	}
	return oc.Caps()
}

// --- schema 方言矩阵 ---

// pathologicalSchemas 是真实会被某些上游拒收的 schema 形态。
//
// 取自客户端实际产出：Claude Code 的工具声明带 $schema 与
// additionalProperties，TypeScript 生成器产出联合 type，MCP 工具常有
// 无参数的空 properties。
var pathologicalSchemas = []struct {
	name string
	raw  string
	// dropped 是各方言下应被剔除的关键字，用于断言「该丢的丢了」。
	dropped []string
}{
	{
		name:    "draft-2020-keywords",
		raw:     `{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"urn:x","type":"object","additionalProperties":false,"title":"Args","properties":{"p":{"type":"string","minLength":1,"maxLength":8,"title":"P"}}}`,
		dropped: []string{"$schema", "$id", "additionalProperties", "title", "minLength", "maxLength"},
	},
	{
		name:    "union-type",
		raw:     `{"type":"object","properties":{"p":{"type":["string","null"]},"q":{"type":["number","integer"]}}}`,
		dropped: nil,
	},
	{
		name:    "numeric-bounds",
		raw:     `{"type":"object","properties":{"n":{"type":"number","exclusiveMinimum":0,"exclusiveMaximum":10},"a":{"type":"array","minItems":1,"maxItems":3,"items":{"type":"string"}}}}`,
		dropped: []string{"exclusiveMinimum", "exclusiveMaximum", "minItems", "maxItems"},
	},
	{
		name:    "nested-deprecated",
		raw:     `{"type":"object","properties":{"outer":{"type":"object","properties":{"inner":{"type":"string","deprecated":true}}}}}`,
		dropped: []string{"deprecated"},
	},
}

// TestSchemaDialectMatrixNormalizesPerProtocol 是 病态 schema × 出站协议。
//
// 按能力位分两档断言而不是按协议名写死清单：方言为空的协议必须字节不变
// （擅自剔除会白丢约束），方言非空的协议必须剔干净且报出丢了什么。
func TestSchemaDialectMatrixNormalizesPerProtocol(t *testing.T) {
	for _, fx := range pathologicalSchemas {
		for _, out := range outboundNames() {
			t.Run(fx.name+"/"+out, func(t *testing.T) {
				caps := capsOf(t, out)
				req := shapeRequest()
				req.Tools = []ir.Tool{{Name: "probe", Schema: fx.raw}}
				body, notes := lossyOf(t, out, req)

				if !json.Valid(body) {
					t.Fatalf("请求体不是合法 JSON: %s", body)
				}
				if caps.SchemaDialect.Empty() {
					// 本协议接受完整 JSON Schema，schema 必须逐字节出现在请求体里。
					// 直接比原文：编码器把 schema 作为 json.RawMessage 原样搬运，
					// 重新解码再编码只会把键按字母重排，断言就永远不成立了。
					if !strings.Contains(string(body), fx.raw) {
						t.Errorf("%s 方言为空，schema 应原样透传\n schema: %s\n body:   %s", out, fx.raw, body)
					}
					if len(notes) != 0 {
						t.Errorf("%s 未做任何归一，不该有说明：%v", out, notes)
					}
					return
				}

				for _, key := range caps.SchemaDialect.Drop {
					if strings.Contains(string(body), `"`+key+`"`) {
						t.Errorf("%s 的方言含 %q，却仍出现在请求体里: %s", out, key, body)
					}
				}
				// 只有真剔掉了关键字才该报有损：大写化与联合折叠是换写法，
				// 报了会让这个协议的每个带工具请求都带一条说明。
				if len(fx.dropped) > 0 {
					if !hasSchemaNote(notes) {
						t.Errorf("%s 剔除了 %v，应报一条 tool schema 说明，实得 %v", out, fx.dropped, notes)
					}
				} else if hasSchemaNote(notes) {
					t.Errorf("%s 只换了写法，不该报 tool schema 说明：%v", out, notes)
				}
			})
		}
	}
}

// TestSchemaDialectMatrixFillsMissingType 断言每个协议都收到带 type 的 schema。
// anthropic 的 input_schema 必填且要求 type，gemini 同样拒收无 type 的 parameters。
func TestSchemaDialectMatrixFillsMissingType(t *testing.T) {
	for _, out := range outboundNames() {
		t.Run(out, func(t *testing.T) {
			req := shapeRequest()
			req.Tools = []ir.Tool{{Name: "probe", Schema: `{"properties":{"p":{"type":"string"}}}`}}
			body, _ := lossyOf(t, out, req)
			if !strings.Contains(string(body), `"type"`) {
				t.Errorf("%s 收到的 schema 缺 type: %s", out, body)
			}
		})
	}
}

// TestSchemaDialectMatrixRejectsInvalidJSONGracefully 断言坏 schema 不打挂整轮。
func TestSchemaDialectMatrixRejectsInvalidJSONGracefully(t *testing.T) {
	for _, out := range outboundNames() {
		t.Run(out, func(t *testing.T) {
			req := shapeRequest()
			req.Tools = []ir.Tool{{Name: "probe", Schema: `{"type":"object",` /* 截断 */}}
			body, notes := lossyOf(t, out, req)
			if !json.Valid(body) {
				t.Fatalf("坏 schema 不该产出非法请求体: %s", body)
			}
			if !hasSchemaNote(notes) {
				t.Errorf("%s 应报 schema 被替换，实得 %v", out, notes)
			}
		})
	}
}

// --- tool_choice 约束矩阵 ---

type toolChoiceCase struct {
	name  string
	build func() *ir.Request
	// verify 拿到请求体与说明，按协议自行断言 tool_choice 的存在性与取值。
	verify func(t *testing.T, out string, body []byte, notes []string)
}

func toolChoiceCases() []toolChoiceCase {
	declared := func() []ir.Tool {
		return []ir.Tool{{Name: "grep", Schema: `{"type":"object","properties":{"p":{"type":"string"}}}`}}
	}
	return []toolChoiceCase{
		{
			name: "choice-without-tools",
			build: func() *ir.Request {
				req := shapeRequest()
				req.ToolChoice = &ir.ToolChoice{Mode: ir.ToolChoiceAuto}
				return req
			},
			verify: func(t *testing.T, out string, body []byte, notes []string) {
				// 上游一律回「tool_choice is only allowed when tools are specified」，
				// 且这是不可重试的 400。
				if hasToolChoiceField(body) {
					t.Errorf("%s 无 tools 时仍写出了 tool_choice: %s", out, body)
				}
				if !hasNoteNaming(notes, "tool_choice") {
					t.Errorf("%s 应报 tool_choice 被丢弃，实得 %v", out, notes)
				}
			},
		},
		{
			name: "named-tool-absent",
			build: func() *ir.Request {
				req := shapeRequest()
				req.Tools = declared()
				req.ToolChoice = &ir.ToolChoice{Mode: ir.ToolChoiceTool, Name: "not_declared"}
				return req
			},
			verify: func(t *testing.T, out string, body []byte, notes []string) {
				if strings.Contains(string(body), "not_declared") {
					t.Errorf("%s 仍指向未声明的工具: %s", out, body)
				}
				if !hasToolChoiceField(body) {
					t.Errorf("%s 应降级为 auto 而非整体丢弃: %s", out, body)
				}
				if !hasNoteNaming(notes, "tool_choice") {
					t.Errorf("%s 应报降级，实得 %v", out, notes)
				}
			},
		},
		{
			name: "named-tool-present",
			build: func() *ir.Request {
				req := shapeRequest()
				req.Tools = declared()
				req.ToolChoice = &ir.ToolChoice{Mode: ir.ToolChoiceTool, Name: "grep"}
				return req
			},
			verify: func(t *testing.T, out string, body []byte, notes []string) {
				if !strings.Contains(string(body), "grep") {
					t.Errorf("%s 丢了合法的具名 tool_choice: %s", out, body)
				}
				if len(notes) != 0 {
					t.Errorf("%s 合法组合不该有说明：%v", out, notes)
				}
			},
		},
		{
			name: "auto-with-tools",
			build: func() *ir.Request {
				req := shapeRequest()
				req.Tools = declared()
				req.ToolChoice = &ir.ToolChoice{Mode: ir.ToolChoiceAuto}
				return req
			},
			verify: func(t *testing.T, out string, body []byte, notes []string) {
				if !hasToolChoiceField(body) {
					t.Errorf("%s 丢了合法的 auto: %s", out, body)
				}
				if len(notes) != 0 {
					t.Errorf("%s 合法组合不该有说明：%v", out, notes)
				}
			},
		},
		{
			name: "none-with-tools",
			build: func() *ir.Request {
				req := shapeRequest()
				req.Tools = declared()
				req.ToolChoice = &ir.ToolChoice{Mode: ir.ToolChoiceNone}
				return req
			},
			verify: func(t *testing.T, out string, body []byte, notes []string) {
				if !hasToolChoiceField(body) {
					t.Errorf("%s 丢了合法的 none: %s", out, body)
				}
				if len(notes) != 0 {
					t.Errorf("%s 合法组合不该有说明：%v", out, notes)
				}
			},
		},
	}
}

func TestToolChoiceMatrixSatisfiesPreconditions(t *testing.T) {
	for _, tc := range toolChoiceCases() {
		for _, out := range outboundNames() {
			t.Run(tc.name+"/"+out, func(t *testing.T) {
				if !capsOf(t, out).Tools {
					// 不支持工具的协议由 DescribeLossy 统一报 tools 被丢，
					// tool_choice 的约束在它身上无从成立。
					t.Fatalf("%s 不支持工具，本矩阵不适用；若新增此类协议应登记协议限制", out)
				}
				body, notes := lossyOf(t, out, tc.build())
				if !json.Valid(body) {
					t.Fatalf("请求体不是合法 JSON: %s", body)
				}
				tc.verify(t, out, body, notes)
			})
		}
	}
}

// --- system 落位矩阵 ---

type systemCase struct {
	name   string
	blocks []ir.Block
	// verify 按 SystemAsText 分两档断言：折字符串的协议看降级文本，
	// 结构化承载 system 的协议看原块是否还在。
	verify func(t *testing.T, out string, caps codec.Capabilities, body []byte, notes []string)
}

func systemCases() []systemCase {
	return []systemCase{
		{
			name: "multi-text",
			blocks: []ir.Block{
				{Type: ir.BlockText, Text: "first"},
				{Type: ir.BlockText, Text: "second"},
			},
			verify: func(t *testing.T, out string, caps codec.Capabilities, body []byte, notes []string) {
				if caps.SystemAsText {
					// 无分隔拼接会把相邻两段粘成 firstsecond。
					if !strings.Contains(string(body), `first\n\nsecond`) {
						t.Errorf("%s 多文本块未以空行分隔: %s", out, body)
					}
				} else if !strings.Contains(string(body), "first") || !strings.Contains(string(body), "second") {
					t.Errorf("%s 丢了 system 文本块: %s", out, body)
				}
				if len(notes) != 0 {
					t.Errorf("%s 纯文本 system 不该有说明：%v", out, notes)
				}
			},
		},
		{
			name: "with-image",
			blocks: []ir.Block{
				{Type: ir.BlockText, Text: "be terse"},
				{Type: ir.BlockImage, Media: &ir.Media{
					MediaType: "image/png",
					Data:      base64.StdEncoding.EncodeToString([]byte("\x89PNG fake")),
					Name:      "ref.png",
				}},
			},
			verify: func(t *testing.T, out string, caps codec.Capabilities, body []byte, notes []string) {
				if !strings.Contains(string(body), "be terse") {
					t.Errorf("%s 丢了 system 文本: %s", out, body)
				}
				if caps.SystemAsText {
					// 单一字符串承载不了图片，必须降级成说明性文本，
					// 否则客户端放在 system 里的截图会被静默吃掉。
					if !strings.Contains(string(body), "ref.png") {
						t.Errorf("%s 的 system 图片被静默丢弃: %s", out, body)
					}
					if !hasNoteNaming(notes, "system media") {
						t.Errorf("%s 应报 system 媒体降级，实得 %v", out, notes)
					}
					return
				}
				// 结构化承载 system 的协议应原生带上图片。
				if !strings.Contains(string(body), "image") {
					t.Errorf("%s 支持结构化 system，图片应原生写出: %s", out, body)
				}
			},
		},
		{
			name:   "empty",
			blocks: []ir.Block{{Type: ir.BlockText, Text: ""}},
			verify: func(t *testing.T, out string, caps codec.Capabilities, body []byte, notes []string) {
				// 空 system 写出空字符串或空 parts 数组会被部分上游拒收。
				for _, field := range []string{`"instructions"`, `"systemInstruction"`, `"system"`} {
					if strings.Contains(string(body), field) {
						t.Errorf("%s 空 system 仍写出了 %s: %s", out, field, body)
					}
				}
			},
		},
	}
}

func TestSystemPlacementMatrixKeepsContent(t *testing.T) {
	for _, sc := range systemCases() {
		for _, out := range outboundNames() {
			t.Run(sc.name+"/"+out, func(t *testing.T) {
				caps := capsOf(t, out)
				req := shapeRequest()
				req.System = sc.blocks
				body, notes := lossyOf(t, out, req)
				if !json.Valid(body) {
					t.Fatalf("请求体不是合法 JSON: %s", body)
				}
				sc.verify(t, out, caps, body, notes)
			})
		}
	}
}

// --- cache 断点矩阵 ---

func TestCacheBreakpointMatrixRespectsBudget(t *testing.T) {
	for _, count := range []int{0, 1, 4, 6} {
		for _, out := range outboundNames() {
			t.Run(breakpointLabel(count)+"/"+out, func(t *testing.T) {
				caps := capsOf(t, out)
				req := shapeRequest()
				req.Messages = nil
				for i := 0; i < count; i++ {
					req.Messages = append(req.Messages, ir.Message{Role: ir.RoleUser,
						Content: []ir.Block{{Type: ir.BlockText, Text: "seg", CacheCtl: "ephemeral"}}})
				}
				if count == 0 {
					req.Messages = shapeRequest().Messages
				}
				body, notes := lossyOf(t, out, req)
				if !json.Valid(body) {
					t.Fatalf("请求体不是合法 JSON: %s", body)
				}
				got := strings.Count(string(body), "cache_control")

				if caps.CacheBreakpoints <= 0 {
					// 不支持断点的协议全清，由 DescribeLossy 统一报一条。
					if got != 0 {
						t.Errorf("%s 不支持断点却写出了 %d 个: %s", out, got, body)
					}
					if count > 0 && !hasNoteNaming(notes, "cache_control") {
						t.Errorf("%s 应报断点被丢弃，实得 %v", out, notes)
					}
					return
				}

				want := count
				if want > caps.CacheBreakpoints {
					want = caps.CacheBreakpoints
				}
				if got != want {
					t.Errorf("%s 断点数 = %d，想要 %d（上限 %d）: %s", out, got, want, caps.CacheBreakpoints, body)
				}
				if count > caps.CacheBreakpoints && !hasNoteNaming(notes, "cache_control") {
					t.Errorf("%s 超上限应报裁剪，实得 %v", out, notes)
				}
				if count <= caps.CacheBreakpoints && len(notes) != 0 {
					t.Errorf("%s 未超上限不该有说明：%v", out, notes)
				}
			})
		}
	}
}

// --- 参数互斥矩阵 ---

// TestThinkingSamplingExclusivityMatrix 按能力位分两档：声明互斥的协议
// 必须清掉采样参数，未声明的必须原样发出。
func TestThinkingSamplingExclusivityMatrix(t *testing.T) {
	for _, out := range outboundNames() {
		t.Run(out, func(t *testing.T) {
			caps := capsOf(t, out)
			if !caps.Thinking {
				t.Fatalf("%s 不支持推理，本矩阵不适用；若新增此类协议应登记协议限制", out)
			}
			temp, topP := 0.7, 0.9
			req := shapeRequest()
			req.Temperature = &temp
			req.TopP = &topP
			req.Thinking = &ir.ThinkingConfig{Enabled: ir.ThinkingOn(), Effort: "medium", BudgetTokens: 4096}
			body, notes := lossyOf(t, out, req)

			hasTemp := strings.Contains(string(body), `"temperature"`)
			hasTopP := strings.Contains(string(body), `"top_p"`) || strings.Contains(string(body), `"topP"`)
			if caps.ThinkingExcludesSampling {
				if hasTemp || hasTopP {
					t.Errorf("%s 声明互斥却仍写出采样参数: %s", out, body)
				}
				if !hasNoteNaming(notes, "temperature/top_p") {
					t.Errorf("%s 应报采样参数被丢弃，实得 %v", out, notes)
				}
				return
			}
			if !hasTemp {
				t.Errorf("%s 未声明互斥，temperature 应照发: %s", out, body)
			}
			if len(notes) != 0 {
				t.Errorf("%s 未声明互斥不该有说明：%v", out, notes)
			}
		})
	}
}

// TestStopSequenceLimitMatrix 按能力位分两档：有上限的协议截断到上限，
// 无上限的协议全部发出（不支持 stop 的由 DescribeLossy 统一报）。
func TestStopSequenceLimitMatrix(t *testing.T) {
	for _, out := range outboundNames() {
		t.Run(out, func(t *testing.T) {
			caps := capsOf(t, out)
			req := shapeRequest()
			req.StopSequences = []string{"a", "b", "c", "d", "e", "f"}
			body, notes := lossyOf(t, out, req)

			if !caps.StopSequences {
				if !hasNoteNaming(notes, "stop_sequences") {
					t.Errorf("%s 不支持 stop，应报丢弃，实得 %v", out, notes)
				}
				return
			}
			got := countStopSequences(t, out, body)
			want := len(req.StopSequences)
			if caps.MaxStopSequences > 0 && want > caps.MaxStopSequences {
				want = caps.MaxStopSequences
				if !hasNoteNaming(notes, "stop_sequences") {
					t.Errorf("%s 超上限应报截断，实得 %v", out, notes)
				}
			} else if len(notes) != 0 {
				t.Errorf("%s 未超上限不该有说明：%v", out, notes)
			}
			if got != want {
				t.Errorf("%s stop 数 = %d，想要 %d: %s", out, got, want, body)
			}
		})
	}
}

// --- 辅助 ---

func breakpointLabel(n int) string {
	switch n {
	case 0:
		return "bp0"
	case 1:
		return "bp1"
	case 4:
		return "bp4"
	default:
		return "bp6"
	}
}

// hasToolChoiceField 覆盖两种线上写法：三家用 tool_choice，gemini 用 toolConfig。
func hasToolChoiceField(body []byte) bool {
	s := string(body)
	return strings.Contains(s, `"tool_choice"`) || strings.Contains(s, `"toolConfig"`)
}

func hasSchemaNote(notes []string) bool {
	return hasNoteNaming(notes, "tool schema")
}

func hasNoteNaming(notes []string, field string) bool {
	for _, n := range notes {
		if strings.Contains(n, field) {
			return true
		}
	}
	return false
}

// countStopSequences 数请求体里的停止序列条数。各协议字段名不同，
// 所以按名字取而不是猜。
func countStopSequences(t *testing.T, out string, body []byte) int {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v", err)
	}
	for _, field := range []string{"stop_sequences", "stop"} {
		if list, ok := obj[field].([]any); ok {
			return len(list)
		}
	}
	// gemini 的停止序列在 generationConfig 里。
	if cfg, ok := obj["generationConfig"].(map[string]any); ok {
		if list, ok := cfg["stopSequences"].([]any); ok {
			return len(list)
		}
	}
	return 0
}
