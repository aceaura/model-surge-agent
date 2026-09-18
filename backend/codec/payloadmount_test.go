package codec_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
)

// 字节预算的挂载靠源码守卫，不靠行为断言。
//
// 原因说清：四协议的 MaxPayloadBytes 当前全为零值（无实测证据，见
// Capabilities 的注释），零值即跳过测量，所以无论挂没挂，行为上都测不出区别
// ——实测过删掉挂载全部测试仍绿。等某个协议填上真上限那天，行为断言才有意义。
//
// 在那之前，这条守卫看的是「代码里有没有这次调用」：漏挂的后果是上限填上了
// 却依然不生效，而那时距离填值可能已隔很久，没人会想起还要挂一次。
func TestEveryOutboundCallsPayloadBudget(t *testing.T) {
	for _, name := range codec.OutboundNames() {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(packageDirOf(t, name), "codec.go")
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			fn := findMethod(file, "EncodeRequestLossy")
			if fn == nil {
				t.Fatalf("%s 里找不到 EncodeRequestLossy", path)
			}
			if !callsIdent(fn, "PayloadBudgetNote") {
				t.Errorf("%s 的 EncodeRequestLossy 未调用 PayloadBudgetNote："+
					"上限填上后预检不会生效", path)
			}
		})
	}
}

// packageDirOf 把协议名映射到包目录。目录名与协议名不完全一致
// （chat_completions → chatcompletions），所以要去掉下划线。
func packageDirOf(t *testing.T, protocol string) string {
	t.Helper()
	return strings.ReplaceAll(protocol, "_", "")
}

func findMethod(file *ast.File, name string) *ast.FuncDecl {
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if ok && fn.Recv != nil && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

func callsIdent(fn *ast.FuncDecl, name string) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if ok && sel.Sel.Name == name {
			found = true
			return false
		}
		return true
	})
	return found
}
