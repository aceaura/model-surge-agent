package gemini_test

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/codec/gemini"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

func toolRequest(t *testing.T, schema string) (body string, notes []string) {
	t.Helper()
	req := &ir.Request{Model: "m",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockText, Text: "hi"},
		}}},
		Tools: []ir.Tool{{Name: "f", Schema: schema}},
	}
	oc, ok := codec.Outbound(gemini.Name)
	if !ok {
		t.Fatal("gemini outbound 未注册")
	}
	lo, ok := oc.(codec.LossyEncoder)
	if !ok {
		t.Fatal("gemini 应实现 LossyEncoder")
	}
	raw, notes, err := lo.EncodeRequestLossy(req)
	if err != nil {
		t.Fatalf("EncodeRequestLossy: %v", err)
	}
	return string(raw), notes
}

// 这一条钉住本轮的入口证据：改成白名单之前，$ref / $defs / oneOf 原样发到
// 上游拿一个 400 Invalid JSON payload，而 DroppedKeys 为空意味着连有损说明
// 都报不出来——排查的人看不到任何线索。
func TestStructuralKeywordsAreDroppedAndReported(t *testing.T) {
	body, notes := toolRequest(t, `{"type":"object","$defs":{"X":{"type":"string"}},`+
		`"properties":{"a":{"$ref":"#/$defs/X"},"b":{"oneOf":[{"type":"string"}]},`+
		`"c":{"allOf":[{"type":"number"}]},"d":{"prefixItems":[{"type":"string"}]}}}`)
	for _, k := range []string{"$ref", "$defs", "oneOf", "allOf", "prefixItems"} {
		if strings.Contains(body, `"`+k+`"`) {
			t.Errorf("关键字 %q 未被剔除: %s", k, body)
		}
	}
	var mentioned bool
	for _, n := range notes {
		if strings.Contains(n, "not in this protocol's dialect") {
			mentioned = true
			for _, k := range []string{"$defs", "$ref", "allOf", "oneOf", "prefixItems"} {
				if !strings.Contains(n, k) {
					t.Errorf("有损说明未点名 %q: %s", k, n)
				}
			}
		}
	}
	if !mentioned {
		t.Errorf("剔除了关键字却没有有损说明: %v", notes)
	}
}

// anyOf 在本协议的方言表内：它是唯一被接受的联合写法。
// 连它一起剔掉会把本来能过的 schema 削成无约束。
func TestAnyOfIsKept(t *testing.T) {
	body, _ := toolRequest(t, `{"type":"object","properties":{"a":{"anyOf":[{"type":"string"}]}}}`)
	if !strings.Contains(body, `"anyOf"`) {
		t.Errorf("anyOf 被剔除了: %s", body)
	}
}

// 数字 enum 在 Gemini 上是 Invalid value at 'enum[0]' (TYPE_STRING)。
func TestNumericEnumBecomesStrings(t *testing.T) {
	body, _ := toolRequest(t, `{"type":"object","properties":{"n":{"type":"integer","enum":[1,2]}}}`)
	if !strings.Contains(body, `"enum":["1","2"]`) {
		t.Errorf("数字 enum 未转成字符串: %s", body)
	}
}

// 四个长度/数量约束刻意仍在剔除之列：参考实现放行它们，但我们原来剔除且
// 上线未见问题，两边冲突时不动已经跑通的行为。这条断言是那个决定的锚点，
// 改动前必须先有实测证据。
func TestLengthConstraintsStayDropped(t *testing.T) {
	body, _ := toolRequest(t, `{"type":"object","properties":{"s":{"type":"string",`+
		`"minLength":1,"maxLength":9},"a":{"type":"array","minItems":1,"maxItems":9}}}`)
	for _, k := range []string{"minLength", "maxLength", "minItems", "maxItems"} {
		if strings.Contains(body, `"`+k+`"`) {
			t.Errorf("关键字 %q 本应仍被剔除: %s", k, body)
		}
	}
}
