package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// shapedBody 走有损编码路径，返回请求体与说明。
func shapedBody(t *testing.T, req *ir.Request) (map[string]any, []string) {
	t.Helper()
	body, notes, err := (outboundCodec{}).EncodeRequestLossy(req)
	if err != nil {
		t.Fatalf("EncodeRequestLossy: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v\n%s", err, body)
	}
	return obj, notes
}

func baseRequest() *ir.Request {
	return &ir.Request{
		Model:     "native",
		MaxTokens: 8192,
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
		},
	}
}

func TestThinkingExcludesSamplingParams(t *testing.T) {
	temp, topP := 0.7, 0.9
	req := baseRequest()
	req.Temperature = &temp
	req.TopP = &topP
	req.Thinking = &ir.ThinkingConfig{Enabled: ir.ThinkingOn(), BudgetTokens: 4096}

	obj, notes := shapedBody(t, req)
	if _, ok := obj["temperature"]; ok {
		t.Errorf("开启推理时不得写出 temperature: %v", obj["temperature"])
	}
	if _, ok := obj["top_p"]; ok {
		t.Errorf("开启推理时不得写出 top_p: %v", obj["top_p"])
	}
	if obj["thinking"] == nil {
		t.Errorf("推理配置不该被丢弃")
	}
	if !hasNote(notes, "temperature/top_p") {
		t.Errorf("未报采样参数被丢弃：%v", notes)
	}
}

func TestSamplingParamsSurviveWithoutThinking(t *testing.T) {
	temp := 0.7
	req := baseRequest()
	req.Temperature = &temp

	obj, notes := shapedBody(t, req)
	if obj["temperature"] == nil {
		t.Errorf("未开推理时 temperature 应照发")
	}
	if len(notes) != 0 {
		t.Errorf("未开推理不该有说明：%v", notes)
	}
}

// TestThinkingDisabledWhenBudgetImpossible 覆盖两个约束无解的情形：
// 预算须同时不低于 1024 且小于 max_tokens。
func TestThinkingDisabledWhenBudgetImpossible(t *testing.T) {
	temp := 0.7
	req := baseRequest()
	req.MaxTokens = 512
	req.Temperature = &temp
	req.Thinking = &ir.ThinkingConfig{Enabled: ir.ThinkingOn(), BudgetTokens: 4096}

	obj, notes := shapedBody(t, req)
	if obj["thinking"] != nil {
		t.Errorf("max_tokens 容不下最小预算时应关掉推理: %v", obj["thinking"])
	}
	// 推理关掉后采样参数就没有互斥对象了，应恢复写出。
	if obj["temperature"] == nil {
		t.Errorf("推理已关，temperature 应恢复写出")
	}
	if !hasNote(notes, "thinking") {
		t.Errorf("未报推理被关：%v", notes)
	}
}

func TestCacheBreakpointsTrimmedToLimit(t *testing.T) {
	req := baseRequest()
	req.Messages = nil
	// 六个断点，超过上限 4。靠后的断点覆盖更长前缀，应被保留。
	for i, text := range []string{"a", "b", "c", "d", "e", "f"} {
		_ = i
		req.Messages = append(req.Messages, ir.Message{Role: ir.RoleUser,
			Content: []ir.Block{{Type: ir.BlockText, Text: text, CacheCtl: "ephemeral"}}})
	}

	obj, notes := shapedBody(t, req)
	kept := countCacheMarks(t, obj)
	if kept != 4 {
		t.Fatalf("断点应裁到 4 个，实得 %d", kept)
	}
	if !hasNote(notes, "cache_control") {
		t.Errorf("未报断点裁剪：%v", notes)
	}
	// 被裁掉的应是最靠前的两个。
	msgs := obj["messages"].([]any)
	for i, want := range []bool{false, false, true, true, true, true} {
		blocks := msgs[i].(map[string]any)["content"].([]any)
		_, has := blocks[0].(map[string]any)["cache_control"]
		if has != want {
			t.Errorf("messages[%d] 断点存在性 = %v，想要 %v", i, has, want)
		}
	}
}

func TestCacheBreakpointsAtLimitUnchanged(t *testing.T) {
	req := baseRequest()
	req.Messages = nil
	for _, text := range []string{"a", "b", "c", "d"} {
		req.Messages = append(req.Messages, ir.Message{Role: ir.RoleUser,
			Content: []ir.Block{{Type: ir.BlockText, Text: text, CacheCtl: "ephemeral"}}})
	}

	obj, notes := shapedBody(t, req)
	if kept := countCacheMarks(t, obj); kept != 4 {
		t.Fatalf("恰达上限时应全部保留，实得 %d", kept)
	}
	if len(notes) != 0 {
		t.Errorf("未超上限不该有说明：%v", notes)
	}
}

func TestToolChoiceDroppedWithoutTools(t *testing.T) {
	req := baseRequest()
	req.ToolChoice = &ir.ToolChoice{Mode: ir.ToolChoiceAuto}

	obj, notes := shapedBody(t, req)
	if _, ok := obj["tool_choice"]; ok {
		t.Errorf("无 tools 时不得写出 tool_choice: %v", obj["tool_choice"])
	}
	if !hasNote(notes, "tool_choice") {
		t.Errorf("未报 tool_choice 被丢弃：%v", notes)
	}
}

func TestNamedToolChoiceDowngradedWhenMissing(t *testing.T) {
	req := baseRequest()
	req.Tools = []ir.Tool{{Name: "grep", Schema: `{"type":"object","properties":{}}`}}
	req.ToolChoice = &ir.ToolChoice{Mode: ir.ToolChoiceTool, Name: "nonexistent"}

	obj, notes := shapedBody(t, req)
	tc, ok := obj["tool_choice"].(map[string]any)
	if !ok {
		t.Fatalf("tool_choice 应降级而非丢弃，实得 %v", obj["tool_choice"])
	}
	if tc["type"] != "auto" {
		t.Errorf("tool_choice 应降级为 auto，实得 %v", tc["type"])
	}
	if !hasNote(notes, "tool_choice") {
		t.Errorf("未报降级：%v", notes)
	}
}

func TestSchemaWithoutTypeIsFilled(t *testing.T) {
	req := baseRequest()
	// 本协议的 input_schema 必填且要求带 type，缺 type 会被拒。
	req.Tools = []ir.Tool{{Name: "grep", Schema: `{"properties":{"p":{"type":"string"}}}`}}

	obj, _ := shapedBody(t, req)
	tools := obj["tools"].([]any)
	schema, ok := tools[0].(map[string]any)["input_schema"].(map[string]any)
	if !ok {
		t.Fatalf("input_schema 必填，实得 %v", tools[0])
	}
	if schema["type"] == nil {
		t.Errorf("缺 type 的 schema 应被补齐，实得 %v", schema)
	}
}

func TestEmptySchemaIsFilled(t *testing.T) {
	req := baseRequest()
	req.Tools = []ir.Tool{{Name: "ping"}}

	obj, _ := shapedBody(t, req)
	tools := obj["tools"].([]any)
	schema, ok := tools[0].(map[string]any)["input_schema"].(map[string]any)
	if !ok {
		t.Fatalf("空 schema 应补成空对象 schema，实得 %v", tools[0])
	}
	if schema["type"] != "object" {
		t.Errorf("补齐的 schema 应为 object，实得 %v", schema["type"])
	}
}

// TestFullJSONSchemaPassesThrough 断言本协议不做方言剔除：
// 它接受完整 JSON Schema，擅自剔除会白丢约束。
func TestFullJSONSchemaPassesThrough(t *testing.T) {
	raw := `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,"properties":{"p":{"type":"string","minLength":1}}}`
	req := baseRequest()
	req.Tools = []ir.Tool{{Name: "grep", Schema: raw}}

	body, notes, err := (outboundCodec{}).EncodeRequestLossy(req)
	if err != nil {
		t.Fatalf("EncodeRequestLossy: %v", err)
	}
	if !strings.Contains(string(body), `"$schema"`) || !strings.Contains(string(body), `"minLength"`) {
		t.Errorf("完整 JSON Schema 应原样透传: %s", body)
	}
	if len(notes) != 0 {
		t.Errorf("无方言剔除不该有说明：%v", notes)
	}
}

func countCacheMarks(t *testing.T, obj map[string]any) int {
	t.Helper()
	count := 0
	msgs, _ := obj["messages"].([]any)
	for _, m := range msgs {
		blocks, _ := m.(map[string]any)["content"].([]any)
		for _, b := range blocks {
			if _, ok := b.(map[string]any)["cache_control"]; ok {
				count++
			}
		}
	}
	return count
}

// 实测：thinking 开启时具名 tool_choice 会被拒（上游原文
// tool_choice 'specified' is incompatible with thinking enabled）。
// 冲突时关推理保工具约束。
func TestForcedToolChoiceDisablesThinking(t *testing.T) {
	temp := 0.7
	req := baseRequest()
	req.Temperature = &temp
	req.Tools = []ir.Tool{{Name: "read", Schema: `{"type":"object","properties":{}}`}}
	req.ToolChoice = &ir.ToolChoice{Mode: ir.ToolChoiceTool, Name: "read"}
	req.Thinking = &ir.ThinkingConfig{Enabled: ir.ThinkingOn(), BudgetTokens: 4096}

	obj, notes := shapedBody(t, req)
	if _, ok := obj["thinking"]; ok {
		t.Errorf("强制工具时推理该被关掉：%v", obj["thinking"])
	}
	if obj["tool_choice"] == nil {
		t.Errorf("工具约束不该被降级")
	}
	// 关了推理就不必再剥采样参数，这是两阶段顺序不可交换的可观测后果。
	if _, ok := obj["temperature"]; !ok {
		t.Errorf("推理已关，temperature 不该被剥离：%v", obj)
	}
	if !hasNote(notes, "forced tool choice") {
		t.Errorf("未报推理因强制工具被关：%v", notes)
	}
	if hasNote(notes, "temperature/top_p") {
		t.Errorf("采样参数未被丢弃却报了说明：%v", notes)
	}
}

func TestAnyToolChoiceDisablesThinking(t *testing.T) {
	req := baseRequest()
	req.Tools = []ir.Tool{{Name: "read", Schema: `{"type":"object","properties":{}}`}}
	req.ToolChoice = &ir.ToolChoice{Mode: ir.ToolChoiceAny}
	req.Thinking = &ir.ThinkingConfig{Enabled: ir.ThinkingOn(), BudgetTokens: 4096}

	obj, notes := shapedBody(t, req)
	if _, ok := obj["thinking"]; ok {
		t.Errorf("any 也是强制，推理该被关掉：%v", obj["thinking"])
	}
	if !hasNote(notes, "forced tool choice") {
		t.Errorf("未报推理因强制工具被关：%v", notes)
	}
}

// auto 与 none 都不强制模型调工具，不与推理冲突。
func TestUnforcedToolChoiceKeepsThinking(t *testing.T) {
	for _, mode := range []ir.ToolChoiceMode{ir.ToolChoiceAuto, ir.ToolChoiceNone} {
		t.Run(string(mode), func(t *testing.T) {
			req := baseRequest()
			req.Tools = []ir.Tool{{Name: "read", Schema: `{"type":"object","properties":{}}`}}
			req.ToolChoice = &ir.ToolChoice{Mode: mode}
			req.Thinking = &ir.ThinkingConfig{Enabled: ir.ThinkingOn(), BudgetTokens: 4096}

			obj, notes := shapedBody(t, req)
			if obj["thinking"] == nil {
				t.Errorf("%s 不是强制，推理不该被关：%v", mode, obj)
			}
			if hasNote(notes, "forced tool choice") {
				t.Errorf("%s 不该触发强制工具阶段：%v", mode, notes)
			}
		})
	}
}

func hasNote(notes []string, want string) bool {
	for _, n := range notes {
		if strings.Contains(n, want) {
			return true
		}
	}
	return false
}
