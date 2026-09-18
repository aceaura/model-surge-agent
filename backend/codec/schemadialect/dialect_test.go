package schemadialect

import (
	"encoding/json"
	"strings"
	"testing"
)

// geminiDialect 与 codec 注册表里 gemini 的方言一致。这里复制一份而不 import
// codec，是为了避免 codec → schemadialect 的依赖反向。
var geminiDialect = Dialect{
	Drop: []string{
		"$schema", "$id", "additionalProperties", "patternProperties",
		"minLength", "maxLength", "minItems", "maxItems",
		"exclusiveMinimum", "exclusiveMaximum", "deprecated", "title",
	},
	UppercaseType:       true,
	CollapseUnionType:   true,
	OmitEmptyProperties: true,
}

func TestEmptyDialectIsIdentity(t *testing.T) {
	raw := []byte(`{"$schema":"x","type":["string","null"],"properties":{}}`)
	got, err := Normalize(raw, Dialect{})
	if err != nil {
		t.Fatalf("空方言不该出错: %v", err)
	}
	if got.Changed || got.Omit || got.Truncated {
		t.Fatalf("空方言必须什么都不做，得到 %+v", got)
	}
	if string(got.Out) != string(raw) {
		t.Fatalf("空方言必须字节不变\n want %s\n got  %s", raw, got.Out)
	}
}

func TestDropKeywordsRecursively(t *testing.T) {
	for _, key := range geminiDialect.Drop {
		t.Run(key, func(t *testing.T) {
			raw := []byte(`{"type":"object","` + key + `":true,"properties":{"a":{"type":"string","` + key + `":true},"b":{"type":"array","items":{"type":"string","` + key + `":true}}}}`)
			got, err := Normalize(raw, geminiDialect)
			if err != nil {
				t.Fatalf("归一化出错: %v", err)
			}
			if !got.Changed {
				t.Fatalf("剔除了关键字却报未改写")
			}
			if strings.Contains(string(got.Out), key) {
				t.Fatalf("关键字 %q 未被递归剔除: %s", key, got.Out)
			}
			// DroppedKeys 是「真丢了约束」的唯一依据，漏记就不会报有损。
			found := false
			for _, k := range got.DroppedKeys {
				if k == key {
					found = true
				}
			}
			if !found {
				t.Fatalf("剔除了 %q 却未记入 DroppedKeys: %v", key, got.DroppedKeys)
			}
		})
	}
}

// TestRewriteOnlyDoesNotReportDrops 断言换写法不算丢约束。
//
// 若大写化也记进 DroppedKeys，gemini 的每个带工具请求都会带一条有损说明，
// 那个字段就再也指不出哪条路由真的削弱了请求。
func TestRewriteOnlyDoesNotReportDrops(t *testing.T) {
	got, err := Normalize([]byte(`{"type":"object","properties":{"a":{"type":["string","null"]}}}`), geminiDialect)
	if err != nil {
		t.Fatalf("归一化出错: %v", err)
	}
	if !got.Changed {
		t.Fatalf("大写化与联合折叠应报改写")
	}
	if len(got.DroppedKeys) != 0 {
		t.Fatalf("纯换写法不该记 DroppedKeys，实得 %v", got.DroppedKeys)
	}
}

func TestCollapseUnionType(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantType string
		wantNull bool
	}{
		{"含 null", `{"type":["string","null"]}`, "STRING", true},
		{"不含 null", `{"type":["string","number"]}`, "STRING", false},
		{"全为 null", `{"type":["null"]}`, "STRING", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Normalize([]byte(tc.in), geminiDialect)
			if err != nil {
				t.Fatalf("归一化出错: %v", err)
			}
			var obj map[string]any
			if err := json.Unmarshal(got.Out, &obj); err != nil {
				t.Fatalf("输出不是合法 JSON: %v", err)
			}
			if obj["type"] != tc.wantType {
				t.Fatalf("type = %v，想要 %q", obj["type"], tc.wantType)
			}
			if nullable, _ := obj["nullable"].(bool); nullable != tc.wantNull {
				t.Fatalf("nullable = %v，想要 %v", obj["nullable"], tc.wantNull)
			}
		})
	}
}

func TestUppercaseTypeOnlyWhenRequested(t *testing.T) {
	raw := []byte(`{"type":"object","properties":{"a":{"type":"string"}}}`)
	up, err := Normalize(raw, geminiDialect)
	if err != nil {
		t.Fatalf("归一化出错: %v", err)
	}
	if !strings.Contains(string(up.Out), `"OBJECT"`) || !strings.Contains(string(up.Out), `"STRING"`) {
		t.Fatalf("type 未大写化: %s", up.Out)
	}

	keep, err := Normalize(raw, Dialect{CollapseUnionType: true})
	if err != nil {
		t.Fatalf("归一化出错: %v", err)
	}
	if keep.Changed || string(keep.Out) != string(raw) {
		t.Fatalf("未开大写化却改写了: %s", keep.Out)
	}
}

func TestOmitEmptyProperties(t *testing.T) {
	got, err := Normalize([]byte(`{"type":"object","properties":{}}`), geminiDialect)
	if err != nil {
		t.Fatalf("归一化出错: %v", err)
	}
	if !got.Omit {
		t.Fatalf("空 properties 应报 Omit")
	}

	kept, err := Normalize([]byte(`{"type":"object","properties":{"a":{"type":"string"}}}`), geminiDialect)
	if err != nil {
		t.Fatalf("归一化出错: %v", err)
	}
	if kept.Omit {
		t.Fatalf("非空 properties 不该报 Omit")
	}
}

func TestDoesNotDescendIntoInstanceValues(t *testing.T) {
	// default 的值是用户数据。它里面恰好有个叫 additionalProperties 的字段，
	// 下钻就会把用户的字段删掉。
	raw := []byte(`{"type":"object","properties":{"cfg":{"type":"object","default":{"additionalProperties":1,"title":"keep me"}}}}`)
	got, err := Normalize(raw, geminiDialect)
	if err != nil {
		t.Fatalf("归一化出错: %v", err)
	}
	if !strings.Contains(string(got.Out), `"additionalProperties":1`) {
		t.Fatalf("default 里的用户字段 additionalProperties 被误删: %s", got.Out)
	}
	if !strings.Contains(string(got.Out), `"keep me"`) {
		t.Fatalf("default 里的用户字段 title 被误删: %s", got.Out)
	}
}

func TestOpaqueKeysAreNotDescended(t *testing.T) {
	for _, key := range []string{"default", "const", "enum", "examples"} {
		t.Run(key, func(t *testing.T) {
			raw := []byte(`{"type":"object","properties":{"a":{"type":"string","` + key + `":{"title":"user value"}}}}`)
			got, err := Normalize(raw, geminiDialect)
			if err != nil {
				t.Fatalf("归一化出错: %v", err)
			}
			if !strings.Contains(string(got.Out), `"user value"`) {
				t.Fatalf("%s 内的用户数据被改写: %s", key, got.Out)
			}
		})
	}
}

func TestDepthCapKeepsSubtreeIntact(t *testing.T) {
	// 造一条超过 maxDepth 的 items 链，末端埋一个 Drop 关键字。
	deep := `{"type":"string","title":"deepest"}`
	for i := 0; i < maxDepth+2; i++ {
		deep = `{"type":"array","items":` + deep + `}`
	}
	got, err := Normalize([]byte(deep), geminiDialect)
	if err != nil {
		t.Fatalf("归一化出错: %v", err)
	}
	if !got.Truncated {
		t.Fatalf("超深 schema 应报 Truncated")
	}
	if !strings.Contains(string(got.Out), `"deepest"`) {
		t.Fatalf("超限子树应原样保留: %s", got.Out)
	}
}

func TestShallowSchemaIsNotTruncated(t *testing.T) {
	got, err := Normalize([]byte(`{"type":"object","properties":{"a":{"type":"array","items":{"type":"string"}}}}`), geminiDialect)
	if err != nil {
		t.Fatalf("归一化出错: %v", err)
	}
	if got.Truncated {
		t.Fatalf("浅 schema 不该报 Truncated")
	}
}

func TestInvalidJSONReturnsError(t *testing.T) {
	got, err := Normalize([]byte(`{not json`), geminiDialect)
	if err == nil {
		t.Fatalf("非法 JSON 应返回 error")
	}
	if string(got.Out) != `{not json` {
		t.Fatalf("出错时应原样返回入参: %s", got.Out)
	}
}

func TestNoChangeKeepsBytes(t *testing.T) {
	// 已是 gemini 方言的 schema 过一遍不该改写。
	raw := []byte(`{"type":"OBJECT","properties":{"a":{"type":"STRING"}}}`)
	got, err := Normalize(raw, geminiDialect)
	if err != nil {
		t.Fatalf("归一化出错: %v", err)
	}
	if got.Changed {
		t.Fatalf("已合方言的 schema 不该报改写")
	}
	if string(got.Out) != string(raw) {
		t.Fatalf("未改写必须字节不变\n want %s\n got  %s", raw, got.Out)
	}
}
