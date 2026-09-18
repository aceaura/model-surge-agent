// Package schemadialect 把工具的 JSON Schema 改写成目标协议能接受的方言。
//
// 独立成包而非塞进 codec：这是纯粹的 JSON 树变换，与协议注册表、能力位、
// 诊断都无关，混进 codec 会让那个包同时承担四件事。
package schemadialect

import (
	"encoding/json"
	"sort"
	"strings"
)

// maxDepth 是下钻深度上限。超限即停止下钻并原样保留该子树。
//
// 真实工具 schema 极少超过 6 层，32 已是两倍以上裕量；设上限是因为
// 病态深度的 schema 会耗尽栈。
const maxDepth = 32

// childKeys 是值本身就是子 schema 的键。
var childKeys = map[string]bool{
	"items":                 true,
	"additionalProperties":  true,
	"contains":              true,
	"not":                   true,
	"if":                    true,
	"then":                  true,
	"else":                  true,
	"propertyNames":         true,
	"unevaluatedItems":      true,
	"unevaluatedProperties": true,
}

// childMapKeys 是值为「名字 → 子 schema」映射的键。
var childMapKeys = map[string]bool{
	"properties":        true,
	"patternProperties": true,
	"$defs":             true,
	"definitions":       true,
	"dependentSchemas":  true,
}

// childListKeys 是值为子 schema 数组的键。
var childListKeys = map[string]bool{
	"oneOf":       true,
	"anyOf":       true,
	"allOf":       true,
	"prefixItems": true,
}

// opaqueKeys 的值是实例数据而不是子 schema，绝不下钻。
//
// 下钻会把用户数据里恰好叫 additionalProperties 的字段删掉——那是用户的
// 值，不是 schema 关键字。三个参考实现都踩过这个坑。
var opaqueKeys = map[string]bool{
	"default":  true,
	"const":    true,
	"enum":     true,
	"examples": true,
}

// Dialect 描述一个目标协议对 JSON Schema 的接受范围。零值表示全盘接受。
type Dialect struct {
	// Drop 是递归剔除的关键字。
	Drop []string
	// UppercaseType 为真时把 type 取值转大写（gemini 的 OBJECT/STRING）。
	UppercaseType bool
	// CollapseUnionType 为真时把 type 联合数组折成首个非 null 成员 + nullable。
	CollapseUnionType bool
	// OmitEmptyProperties 为真时顶层 properties 为空对象则整体省略。
	OmitEmptyProperties bool
}

// Empty 判断本方言是否什么都不改。
func (d Dialect) Empty() bool {
	return len(d.Drop) == 0 && !d.UppercaseType && !d.CollapseUnionType && !d.OmitEmptyProperties
}

// Result 是一次归一化的结果。
type Result struct {
	// Out 是改写后的字节。Changed 为假时与入参逐字节相同。
	Out []byte
	// Changed 报告是否实际改写。未改写必须原样返回：请求体漂移会打掉
	// 上游的 prompt cache 前缀。
	Changed bool
	// DroppedKeys 是被剔除的关键字，已去重排序。
	//
	// 与 Changed 分开是因为两者语义不同：type 大写化与联合折叠是把同一
	// 约束换个写法，没丢任何信息；剔除 minLength 才是真把一条约束弄没了。
	// 只有后者值得报进有损诊断——否则 gemini 的每个带工具请求都会带一条
	// 说明，那个字段就再也指不出哪条路由真的削弱了请求。
	DroppedKeys []string
	// Omit 为真表示该 schema 应整体省略（对应 OmitEmptyProperties）。
	Omit bool
	// Truncated 为真表示有子树因超过深度上限而被原样保留。
	Truncated bool
}

// Normalize 按方言改写 schema。
//
// raw 非法 JSON 时返回 error，由调用方决定降级方式——本包不替调用方
// 编造一份 schema。
func Normalize(raw []byte, d Dialect) (Result, error) {
	if d.Empty() {
		return Result{Out: raw}, nil
	}
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return Result{Out: raw}, err
	}
	st := &state{dialect: d, drop: dropSet(d.Drop)}
	walked := st.walk(root, 0)

	obj, isObj := walked.(map[string]any)
	if isObj && d.OmitEmptyProperties {
		if props, ok := obj["properties"].(map[string]any); ok && len(props) == 0 {
			return Result{Out: raw, DroppedKeys: st.droppedKeys(), Omit: true, Truncated: st.truncated}, nil
		}
	}

	out, err := json.Marshal(walked)
	if err != nil {
		return Result{Out: raw}, err
	}
	if !st.changed {
		// 没改写就返回原字节：重新 Marshal 会重排键序、改变空白，
		// 这本身就是请求体漂移。
		return Result{Out: raw, Truncated: st.truncated}, nil
	}
	return Result{Out: out, Changed: true, DroppedKeys: st.droppedKeys(), Truncated: st.truncated}, nil
}

type state struct {
	dialect   Dialect
	drop      map[string]bool
	dropped   map[string]bool
	changed   bool
	truncated bool
}

func (s *state) droppedKeys() []string {
	if len(s.dropped) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.dropped))
	for k := range s.dropped {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func dropSet(keys []string) map[string]bool {
	if len(keys) == 0 {
		return nil
	}
	s := make(map[string]bool, len(keys))
	for _, k := range keys {
		s[k] = true
	}
	return s
}

func (s *state) walk(node any, depth int) any {
	obj, ok := node.(map[string]any)
	if !ok {
		return node
	}
	if depth >= maxDepth {
		s.truncated = true
		return node
	}

	for k := range obj {
		if s.drop[k] {
			delete(obj, k)
			s.changed = true
			if s.dropped == nil {
				s.dropped = map[string]bool{}
			}
			s.dropped[k] = true
		}
	}
	s.normalizeType(obj)

	for _, k := range sortedKeys(obj) {
		if opaqueKeys[k] {
			continue
		}
		v := obj[k]
		switch {
		case childKeys[k]:
			obj[k] = s.walk(v, depth+1)
		case childMapKeys[k]:
			if m, ok := v.(map[string]any); ok {
				for _, name := range sortedKeys(m) {
					m[name] = s.walk(m[name], depth+1)
				}
			}
		case childListKeys[k]:
			if list, ok := v.([]any); ok {
				for i := range list {
					list[i] = s.walk(list[i], depth+1)
				}
			}
		}
	}
	return obj
}

// normalizeType 折叠联合 type 并按方言大写化。
func (s *state) normalizeType(obj map[string]any) {
	raw, ok := obj["type"]
	if !ok {
		return
	}
	if list, isList := raw.([]any); isList && s.dialect.CollapseUnionType {
		picked, hasNull := pickType(list)
		obj["type"] = picked
		if hasNull {
			obj["nullable"] = true
		}
		s.changed = true
		raw = picked
	}
	if str, isStr := raw.(string); isStr && s.dialect.UppercaseType {
		if up := strings.ToUpper(str); up != str {
			obj["type"] = up
			s.changed = true
		}
	}
}

// pickType 取首个非 null 成员，并报告联合里是否含 null。
// 全为 null 时退成 string——协议要求 type 必填且不接受 "null"。
func pickType(list []any) (string, bool) {
	hasNull := false
	picked := ""
	for _, item := range list {
		str, ok := item.(string)
		if !ok {
			continue
		}
		if strings.EqualFold(str, "null") {
			hasNull = true
			continue
		}
		if picked == "" {
			picked = str
		}
	}
	if picked == "" {
		picked = "string"
	}
	return picked, hasNull
}

// sortedKeys 让遍历顺序确定，否则 truncated 之类的标志会随 map 序抖动。
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
