package gemini

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

func toolParameters(t *testing.T, obj map[string]any) (map[string]any, bool) {
	t.Helper()
	tools, ok := obj["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Fatalf("tools 未写出：%v", obj["tools"])
	}
	decls, ok := tools[0].(map[string]any)["functionDeclarations"].([]any)
	if !ok || len(decls) == 0 {
		t.Fatalf("functionDeclarations 未写出：%v", tools[0])
	}
	params, present := decls[0].(map[string]any)["parameters"]
	if !present {
		return nil, false
	}
	return params.(map[string]any), true
}

// TestDialectDropsUnsupportedKeywords 覆盖本协议最常见的 400 来源：
// 它的 schema 是 OpenAPI 子集，表外关键字一律被当未知字段拒收。
func TestDialectDropsUnsupportedKeywords(t *testing.T) {
	raw := `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,"title":"Args","properties":{"p":{"type":"string","minLength":1,"maxLength":9,"title":"P"}}}`
	req := shapeBaseRequest()
	req.Tools = []ir.Tool{{Name: "grep", Schema: raw}}

	body, notes, err := (outboundCodec{}).EncodeRequestLossy(req)
	if err != nil {
		t.Fatalf("EncodeRequestLossy: %v", err)
	}
	for _, key := range []string{"$schema", "additionalProperties", "title", "minLength", "maxLength"} {
		if strings.Contains(string(body), `"`+key+`"`) {
			t.Errorf("关键字 %q 未被剔除: %s", key, body)
		}
	}
	if !hasShapeNote(notes, "tool schema") {
		t.Errorf("剔除关键字应报有损：%v", notes)
	}
}

func TestDialectUppercasesTypes(t *testing.T) {
	req := shapeBaseRequest()
	req.Tools = []ir.Tool{{Name: "grep",
		Schema: `{"type":"object","properties":{"p":{"type":"string"}}}`}}

	obj, notes := shapedBody(t, req)
	params, ok := toolParameters(t, obj)
	if !ok {
		t.Fatalf("parameters 应写出")
	}
	if params["type"] != "OBJECT" {
		t.Errorf("顶层 type 应大写，实得 %v", params["type"])
	}
	props := params["properties"].(map[string]any)
	if props["p"].(map[string]any)["type"] != "STRING" {
		t.Errorf("子 schema type 应大写，实得 %v", props["p"])
	}
	// 大写化只是换写法，没丢约束，不该报有损。
	if len(notes) != 0 {
		t.Errorf("纯换写法不该报有损：%v", notes)
	}
}

func TestDialectCollapsesUnionType(t *testing.T) {
	req := shapeBaseRequest()
	req.Tools = []ir.Tool{{Name: "grep",
		Schema: `{"type":"object","properties":{"p":{"type":["string","null"]}}}`}}

	obj, _ := shapedBody(t, req)
	params, _ := toolParameters(t, obj)
	p := params["properties"].(map[string]any)["p"].(map[string]any)
	if p["type"] != "STRING" {
		t.Errorf("联合 type 应折成首个非 null 成员，实得 %v", p["type"])
	}
	if p["nullable"] != true {
		t.Errorf("含 null 的联合应置 nullable，实得 %v", p["nullable"])
	}
}

func TestEmptyPropertiesOmitsParameters(t *testing.T) {
	req := shapeBaseRequest()
	// 本协议拒收 properties 为空对象的 parameters。
	req.Tools = []ir.Tool{{Name: "ping", Schema: `{"type":"object","properties":{}}`}}

	obj, notes := shapedBody(t, req)
	if _, present := toolParameters(t, obj); present {
		t.Errorf("空 properties 时应整体省略 parameters")
	}
	// 没有参数的工具省掉 parameters 没丢任何约束。
	if len(notes) != 0 {
		t.Errorf("省略空 parameters 不该报有损：%v", notes)
	}
}

func TestSystemMediaDowngradedIntoInstruction(t *testing.T) {
	req := shapeBaseRequest()
	req.System = []ir.Block{
		{Type: ir.BlockText, Text: "be terse"},
		{Type: ir.BlockImage, Media: &ir.Media{
			MediaType: "image/png",
			Data:      base64.StdEncoding.EncodeToString([]byte("\x89PNG fake")),
			Name:      "ref.png",
		}},
	}

	obj, notes := shapedBody(t, req)
	si, ok := obj["systemInstruction"].(map[string]any)
	if !ok {
		t.Fatalf("systemInstruction 应写出：%v", obj["systemInstruction"])
	}
	text := si["parts"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "be terse") {
		t.Errorf("文本块丢失：%q", text)
	}
	// 本协议的 systemInstruction 只承载文本，图片必须降级成说明性文本，
	// 否则客户端放在 system 里的截图会被静默吃掉。
	if !strings.Contains(text, "ref.png") {
		t.Errorf("system 里的图片被静默丢弃：%q", text)
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
	si := obj["systemInstruction"].(map[string]any)
	text := si["parts"].([]any)[0].(map[string]any)["text"].(string)
	if text != "first\n\nsecond" {
		t.Errorf("多文本块应以空行分隔，实得 %q", text)
	}
}

func TestEmptySystemOmitsInstruction(t *testing.T) {
	req := shapeBaseRequest()
	req.System = []ir.Block{{Type: ir.BlockText, Text: ""}}

	obj, _ := shapedBody(t, req)
	if _, ok := obj["systemInstruction"]; ok {
		t.Errorf("空 system 不该写出 systemInstruction：%v", obj["systemInstruction"])
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
