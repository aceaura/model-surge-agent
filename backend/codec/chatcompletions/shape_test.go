package chatcompletions

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

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

func shapeBaseRequest() *ir.Request {
	return &ir.Request{
		Model:     "native",
		MaxTokens: 8192,
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
		},
	}
}

func TestStopSequencesTruncatedToLimit(t *testing.T) {
	req := shapeBaseRequest()
	req.StopSequences = []string{"a", "b", "c", "d", "e", "f"}

	obj, notes := shapedBody(t, req)
	stops, ok := obj["stop"].([]any)
	if !ok {
		t.Fatalf("stop 应写出，实得 %v", obj["stop"])
	}
	if len(stops) != 4 {
		t.Fatalf("stop 应截断到 4 项，实得 %d", len(stops))
	}
	// 截断保留前 4 项：停止序列无先后覆盖关系，保前者即客户端的声明顺序。
	for i, want := range []string{"a", "b", "c", "d"} {
		if stops[i] != want {
			t.Errorf("stop[%d] = %v，想要 %q", i, stops[i], want)
		}
	}
	if !hasShapeNote(notes, "stop_sequences") {
		t.Errorf("未报截断：%v", notes)
	}
}

func TestStopSequencesAtLimitUnchanged(t *testing.T) {
	req := shapeBaseRequest()
	req.StopSequences = []string{"a", "b", "c", "d"}

	obj, notes := shapedBody(t, req)
	if stops := obj["stop"].([]any); len(stops) != 4 {
		t.Fatalf("恰达上限时应全留，实得 %d", len(stops))
	}
	if len(notes) != 0 {
		t.Errorf("未超上限不该有说明：%v", notes)
	}
}

func TestToolChoiceDroppedWithoutTools(t *testing.T) {
	req := shapeBaseRequest()
	req.ToolChoice = &ir.ToolChoice{Mode: ir.ToolChoiceAuto}

	obj, notes := shapedBody(t, req)
	if _, ok := obj["tool_choice"]; ok {
		t.Errorf("上游要求 tool_choice 必须伴随 tools，实得 %v", obj["tool_choice"])
	}
	if !hasShapeNote(notes, "tool_choice") {
		t.Errorf("未报丢弃：%v", notes)
	}
}

func TestNamedToolChoiceDowngradedWhenMissing(t *testing.T) {
	req := shapeBaseRequest()
	req.Tools = []ir.Tool{{Name: "grep", Schema: `{"type":"object","properties":{}}`}}
	req.ToolChoice = &ir.ToolChoice{Mode: ir.ToolChoiceTool, Name: "absent"}

	obj, notes := shapedBody(t, req)
	if obj["tool_choice"] != "auto" {
		t.Errorf("指向未声明工具应降级 auto，实得 %v", obj["tool_choice"])
	}
	if !hasShapeNote(notes, "tool_choice") {
		t.Errorf("未报降级：%v", notes)
	}
}

func TestNamedToolChoiceKeptWhenPresent(t *testing.T) {
	req := shapeBaseRequest()
	req.Tools = []ir.Tool{{Name: "grep", Schema: `{"type":"object","properties":{}}`}}
	req.ToolChoice = &ir.ToolChoice{Mode: ir.ToolChoiceTool, Name: "grep"}

	obj, notes := shapedBody(t, req)
	tc, ok := obj["tool_choice"].(map[string]any)
	if !ok {
		t.Fatalf("具名 tool_choice 应保留，实得 %v", obj["tool_choice"])
	}
	fn, _ := tc["function"].(map[string]any)
	if fn["name"] != "grep" {
		t.Errorf("具名工具存在时不该改写，实得 %v", tc)
	}
	if len(notes) != 0 {
		t.Errorf("合法组合不该有说明：%v", notes)
	}
}

// TestFullJSONSchemaPassesThrough 断言本协议不做方言剔除。
func TestFullJSONSchemaPassesThrough(t *testing.T) {
	raw := `{"$schema":"x","type":"object","additionalProperties":false,"properties":{"p":{"type":["string","null"]}}}`
	req := shapeBaseRequest()
	req.Tools = []ir.Tool{{Name: "grep", Schema: raw}}

	body, notes, err := (outboundCodec{}).EncodeRequestLossy(req)
	if err != nil {
		t.Fatalf("EncodeRequestLossy: %v", err)
	}
	if !strings.Contains(string(body), `"additionalProperties"`) {
		t.Errorf("完整 JSON Schema 应原样透传: %s", body)
	}
	if !strings.Contains(string(body), `["string","null"]`) {
		t.Errorf("联合 type 应原样透传: %s", body)
	}
	if len(notes) != 0 {
		t.Errorf("无方言剔除不该有说明：%v", notes)
	}
}

// 实测本协议允许推理与强制工具共存（deepseek 上具名 tool_choice + 思考
// 开启回 200）。能力位为假，这一阶段不该触发。
func TestForcedToolChoiceKeepsThinking(t *testing.T) {
	req := shapeBaseRequest()
	req.Tools = []ir.Tool{{Name: "read", Schema: `{"type":"object","properties":{}}`}}
	req.ToolChoice = &ir.ToolChoice{Mode: ir.ToolChoiceTool, Name: "read"}
	req.Thinking = &ir.ThinkingConfig{Enabled: true, BudgetTokens: 4096}

	obj, notes := shapedBody(t, req)
	if obj["reasoning_effort"] == nil {
		t.Errorf("本协议允许推理与强制工具共存，推理不该被关：%v", obj)
	}
	if obj["tool_choice"] == nil {
		t.Errorf("工具约束不该被降级")
	}
	if hasShapeNote(notes, "forced tool choice") {
		t.Errorf("本协议不该触发强制工具阶段：%v", notes)
	}
}

func hasShapeNote(notes []string, want string) bool {
	for _, n := range notes {
		if strings.Contains(n, want) {
			return true
		}
	}
	return false
}
