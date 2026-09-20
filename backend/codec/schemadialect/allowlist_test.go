package schemadialect_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec/schemadialect"
)

// 白名单存在的理由：黑名单永远追不全。这一条钉住四个结构性关键字——
// 它们在改成白名单之前全都原样发出去，且 DroppedKeys 为空，
// 连有损说明都报不出来。
func TestAllowlistDropsStructuralKeywords(t *testing.T) {
	raw := []byte(`{"type":"object","$ref":"#/$defs/X","$defs":{"X":{"type":"string"}},` +
		`"oneOf":[{"type":"string"}],"allOf":[{"type":"number"}],` +
		`"prefixItems":[{"type":"string"}],"properties":{"a":{"type":"string"}}}`)
	d := schemadialect.Dialect{Allow: []string{"type", "properties"}}
	res, err := schemadialect.Normalize(raw, d)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if !res.Changed {
		t.Fatal("剔除了关键字却报 Changed=false")
	}
	for _, k := range []string{"$ref", "$defs", "oneOf", "allOf", "prefixItems"} {
		if strings.Contains(string(res.Out), `"`+k+`"`) {
			t.Errorf("表外关键字 %q 仍在输出里: %s", k, res.Out)
		}
	}
	want := []string{"$defs", "$ref", "allOf", "oneOf", "prefixItems"}
	if !reflect.DeepEqual(res.DroppedKeys, want) {
		t.Errorf("DroppedKeys = %v，想要 %v", res.DroppedKeys, want)
	}
	// 表内的键必须留下，否则白名单就成了「全删」。
	if !strings.Contains(string(res.Out), `"properties"`) {
		t.Errorf("表内关键字 properties 被剔除了: %s", res.Out)
	}
}

// 剔除必须排在下钻之前：容器被剔掉之后再下钻会把子树里的键也记进
// DroppedKeys，说明就指不出真正被削掉的是哪一条约束。
func TestDroppedContainerSubtreeIsNotReported(t *testing.T) {
	raw := []byte(`{"type":"object","$defs":{"X":{"type":"string","pattern":"^a$"}},` +
		`"properties":{"a":{"type":"string"}}}`)
	d := schemadialect.Dialect{Allow: []string{"type", "properties"}}
	res, err := schemadialect.Normalize(raw, d)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	for _, k := range res.DroppedKeys {
		if k == "pattern" {
			t.Errorf("子树里的 pattern 被记进了 DroppedKeys：%v", res.DroppedKeys)
		}
	}
}

// Allow 与 Drop 并存：前者说「表外都不认」，后者说「这个键我明确知道不行」。
// 两者都填时表内被 Drop 点名的键仍要剔除——否则 gemini 那四个长度约束
// （放行有风险、剔除已跑通）会被白名单悄悄放回去。
func TestAllowAndDropCoexist(t *testing.T) {
	raw := []byte(`{"type":"string","pattern":"^a$","format":"email"}`)
	d := schemadialect.Dialect{
		Allow: []string{"type", "pattern", "format"},
		Drop:  []string{"pattern"},
	}
	res, err := schemadialect.Normalize(raw, d)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if strings.Contains(string(res.Out), `"pattern"`) {
		t.Errorf("Drop 点名的键仍在输出里: %s", res.Out)
	}
	if !strings.Contains(string(res.Out), `"format"`) {
		t.Errorf("表内未被点名的键被剔除了: %s", res.Out)
	}
}

// Empty() 认不认新字段决定 Normalize 会不会直接原样返回。
// 漏一个就是「填了方言但什么都没生效」这种最难查的静默失效。
func TestEmptyRecognizesEveryDimension(t *testing.T) {
	cases := map[string]schemadialect.Dialect{
		"drop":           {Drop: []string{"x"}},
		"allow":          {Allow: []string{"type"}},
		"stringEnumOnly": {StringEnumOnly: true},
		"uppercaseType":  {UppercaseType: true},
		"collapseUnion":  {CollapseUnionType: true},
		"omitEmptyProps": {OmitEmptyProperties: true},
	}
	for name, d := range cases {
		if d.Empty() {
			t.Errorf("%s: 非零方言被 Empty() 判成什么都不改", name)
		}
	}
	if !(schemadialect.Dialect{}).Empty() {
		t.Error("零值方言应当是 Empty")
	}
}

// enum 成员的四种标量形态都要能转，且字面量要与原 JSON 一致。
func TestEnumMembersBecomeStringLiterals(t *testing.T) {
	raw := []byte(`{"type":"string","enum":[1,2.5,true,null,"x",1000000]}`)
	d := schemadialect.Dialect{StringEnumOnly: true}
	res, err := schemadialect.Normalize(raw, d)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	var got struct {
		Enum []any `json:"enum"`
	}
	if err := json.Unmarshal(res.Out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// 1000000 必须是 "1000000" 而不是 "1e+06"：模型按字符串匹配可选值，
	// 科学记数法与原 schema 里的字面量对不上。
	want := []any{"1", "2.5", "true", "null", "x", "1000000"}
	if !reflect.DeepEqual(got.Enum, want) {
		t.Errorf("enum = %#v，想要 %#v", got.Enum, want)
	}
}

// 已经全是字符串的 enum 不该触发改写：请求体漂移会打掉上游的 prompt cache 前缀。
func TestAllStringEnumIsLeftByteIdentical(t *testing.T) {
	raw := []byte(`{"type":"string","enum":["a","b"]}`)
	res, err := schemadialect.Normalize(raw, schemadialect.Dialect{StringEnumOnly: true})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if res.Changed {
		t.Errorf("全字符串 enum 被判成改写过: %s", res.Out)
	}
	if string(res.Out) != string(raw) {
		t.Errorf("输出与入参不同: got %s want %s", res.Out, raw)
	}
}

// 对象或数组成员转不成标量字面量：整条删掉并报进 DroppedKeys。
// 留一个半截的枚举比没有约束更坏——模型会以为可选值只有能转的那几个。
func TestNonScalarEnumMemberDropsWholeEnum(t *testing.T) {
	raw := []byte(`{"type":"string","enum":["a",{"nested":1}]}`)
	res, err := schemadialect.Normalize(raw, schemadialect.Dialect{StringEnumOnly: true})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if strings.Contains(string(res.Out), `"enum"`) {
		t.Errorf("含非标量成员的 enum 应整条删除: %s", res.Out)
	}
	if !reflect.DeepEqual(res.DroppedKeys, []string{"enum"}) {
		t.Errorf("DroppedKeys = %v，想要 [enum]", res.DroppedKeys)
	}
}

// enum 仍在「绝不下钻」之列：成员是实例数据，恰好叫 properties 的键不该被当
// 成子 schema 走一遍变换。
func TestEnumMembersAreNotWalkedAsSchemas(t *testing.T) {
	raw := []byte(`{"type":"object","enum":[{"type":"lowercase","$ref":"keep"}]}`)
	d := schemadialect.Dialect{Allow: []string{"type", "enum"}, UppercaseType: true}
	res, err := schemadialect.Normalize(raw, d)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	// 成员是对象 → StringEnumOnly 未开时应原样保留，且里面的 type 不被大写化、
	// $ref 不被白名单剔除。
	if !strings.Contains(string(res.Out), `"lowercase"`) {
		t.Errorf("enum 成员里的实例数据被当成 schema 改写了: %s", res.Out)
	}
	if !strings.Contains(string(res.Out), `"$ref"`) {
		t.Errorf("enum 成员里的键被白名单剔除了: %s", res.Out)
	}
}

// 嵌套层里的白名单同样生效：只治顶层等于没治，工具 schema 的约束几乎都在
// properties 底下。
func TestAllowlistAppliesAtEveryDepth(t *testing.T) {
	raw := []byte(`{"type":"object","properties":{"a":{"type":"array",` +
		`"items":{"type":"string","$ref":"nope"}}}}`)
	d := schemadialect.Dialect{Allow: []string{"type", "properties", "items"}}
	res, err := schemadialect.Normalize(raw, d)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if strings.Contains(string(res.Out), `"$ref"`) {
		t.Errorf("嵌套层的表外关键字未被剔除: %s", res.Out)
	}
}
