package store

import (
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/cache"
	"github.com/aceaura/model-surge-agent/backend/contract/agentv1"
)

// 用量维度在三层各自声明：明细表的列、实时摘要的字段、对外契约的字段。
// 任何一层少一维，两个视图就长期对不上而两侧都不报错——这正是本轮要消除的
// 那类故障，也是最容易在「新增一维」时漏掉的地方。
//
// 从 schema.sql 里解列名而不是手写一份清单：手写的清单跟 schema 一样会漏，
// 而漏的方向恰好一致（都忘了新增那维），于是断言与被测对象一起错。
func TestUsageDimensionsAgreeAcrossLayers(t *testing.T) {
	columns := usageColumnsInSchema(t)
	if len(columns) < 5 {
		t.Fatalf("schema.sql 里只解出 %d 个用量列（%v）——解析失效了，这组断言等于没测",
			len(columns), columns)
	}

	for _, layer := range []struct {
		name string
		typ  reflect.Type
	}{
		{"cache.LiveEntry", reflect.TypeOf(cache.LiveEntry{})},
		{"agentv1.LiveEntry", reflect.TypeOf(agentv1.LiveEntry{})},
	} {
		t.Run(layer.name, func(t *testing.T) {
			got := usageJSONTags(layer.typ)
			if !reflect.DeepEqual(got, columns) {
				t.Errorf("%s 的用量字段 = %v\n明细表的用量列   = %v\n"+
					"两侧必须逐维相等：少一维就是 /admin/live 与 /admin/requests 静默分叉",
					layer.name, got, columns)
			}
		})
	}
}

// usageColumnsInSchema 解出 request_log 里所有以 _tokens 结尾的列名。
func usageColumnsInSchema(t *testing.T) []string {
	t.Helper()
	ddl, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	// 同时覆盖 CREATE TABLE 里的列与 ALTER TABLE ADD COLUMN 补上的列：
	// 老库走的是后一条路径，只扫前者会漏掉升级加的维度。
	re := regexp.MustCompile(`(?m)^\s*(?:ALTER TABLE request_log ADD COLUMN IF NOT EXISTS\s+)?([a-z_]+_tokens)\b`)
	seen := map[string]bool{}
	var out []string
	for _, m := range re.FindAllStringSubmatch(string(ddl), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

// usageJSONTags 取出结构体里所有 json 名以 _tokens 结尾的字段名。
func usageJSONTags(typ reflect.Type) []string {
	var out []string
	for i := 0; i < typ.NumField(); i++ {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if strings.HasSuffix(name, "_tokens") {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
