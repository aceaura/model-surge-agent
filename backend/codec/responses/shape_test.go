package responses

import (
	"encoding/base64"
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

func TestSystemMediaDowngradedIntoInstructions(t *testing.T) {
	req := shapeBaseRequest()
	req.System = []ir.Block{
		{Type: ir.BlockText, Text: "be terse"},
		{Type: ir.BlockDocument, Media: &ir.Media{
			MediaType: "application/pdf",
			Data:      base64.StdEncoding.EncodeToString([]byte("%PDF fake")),
			Name:      "spec.pdf",
		}},
	}

	obj, notes := shapedBody(t, req)
	instr, ok := obj["instructions"].(string)
	if !ok {
		t.Fatalf("instructions 应写出：%v", obj["instructions"])
	}
	if !strings.Contains(instr, "be terse") {
		t.Errorf("文本块丢失：%q", instr)
	}
	// instructions 是单一字符串，附件必须降级成说明性文本，
	// 否则客户端放在 system 里的文件会被静默吃掉。
	if !strings.Contains(instr, "spec.pdf") {
		t.Errorf("system 里的附件被静默丢弃：%q", instr)
	}
	if !hasShapeNote(notes, "system media") {
		t.Errorf("未报 system 媒体降级：%v", notes)
	}
}

func TestSystemTextBlocksJoinedWithBlankLine(t *testing.T) {
	req := shapeBaseRequest()
	req.System = []ir.Block{
		{Type: ir.BlockText, Text: "first"},
		{Type: ir.BlockText, Text: "second"},
	}

	obj, _ := shapedBody(t, req)
	// 无分隔拼接会把相邻两段粘成一句 "firstsecond"。
	if obj["instructions"] != "first\n\nsecond" {
		t.Errorf("多文本块应以空行分隔，实得 %q", obj["instructions"])
	}
}

func TestEmptySystemOmitsInstructions(t *testing.T) {
	req := shapeBaseRequest()
	req.System = []ir.Block{{Type: ir.BlockText, Text: ""}}

	obj, _ := shapedBody(t, req)
	if _, ok := obj["instructions"]; ok {
		t.Errorf("空 system 不该写出 instructions：%v", obj["instructions"])
	}
}

func TestToolChoiceDroppedWithoutTools(t *testing.T) {
	req := shapeBaseRequest()
	req.ToolChoice = &ir.ToolChoice{Mode: ir.ToolChoiceAny}

	obj, notes := shapedBody(t, req)
	if _, ok := obj["tool_choice"]; ok {
		t.Errorf("无 tools 时不得写出 tool_choice：%v", obj["tool_choice"])
	}
	if !hasShapeNote(notes, "tool_choice") {
		t.Errorf("未报丢弃：%v", notes)
	}
}

// TestFullJSONSchemaPassesThrough 断言本协议不做方言剔除。
func TestFullJSONSchemaPassesThrough(t *testing.T) {
	raw := `{"$schema":"x","type":"object","additionalProperties":false,"properties":{"p":{"type":"string","minLength":1}}}`
	req := shapeBaseRequest()
	req.Tools = []ir.Tool{{Name: "grep", Schema: raw}}

	body, notes, err := (outboundCodec{}).EncodeRequestLossy(req)
	if err != nil {
		t.Fatalf("EncodeRequestLossy: %v", err)
	}
	if !strings.Contains(string(body), `"minLength"`) {
		t.Errorf("完整 JSON Schema 应原样透传: %s", body)
	}
	if len(notes) != 0 {
		t.Errorf("无方言剔除不该有说明：%v", notes)
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
