package httpapi

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 本文件守的是包装 ResponseWriter 的类型必须实现 Unwrap。
//
// 这条缺了不会报错、不会有症状：http.NewResponseController 沿 Unwrap 链
// 找真实连接，找不到就让 SetWriteDeadline 返回 ErrNotSupported，而数据面
// 对那个返回值是忽略的（没有合理的补救动作）。于是慢客户端的写 deadline
// 静默失效，一个不读的客户端就能无限占住一条上游连接。

// deadlineThrough 在真实连接上试着推写 deadline，回报是否成功。
func deadlineThrough(t *testing.T, wrap func(http.ResponseWriter, *http.Request) http.ResponseWriter) error {
	t.Helper()
	errs := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wrapped := wrap(w, r)
		errs <- http.NewResponseController(wrapped).
			SetWriteDeadline(time.Now().Add(time.Second))
		_, _ = wrapped.Write([]byte("ok"))
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	resp.Body.Close()
	return <-errs
}

func TestStatusRecorderPassesWriteDeadlineThrough(t *testing.T) {
	if err := deadlineThrough(t, func(w http.ResponseWriter, _ *http.Request) http.ResponseWriter {
		return &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	}); err != nil {
		t.Errorf("SetWriteDeadline 穿不过 statusRecorder：%v，"+
			"慢客户端的写 deadline 会静默失效", err)
	}
}

func TestRejectWriterPassesWriteDeadlineThrough(t *testing.T) {
	if err := deadlineThrough(t, func(w http.ResponseWriter, r *http.Request) http.ResponseWriter {
		return &rejectWriter{ResponseWriter: w, server: &Server{}, req: r}
	}); err != nil {
		t.Errorf("SetWriteDeadline 穿不过 rejectWriter：%v", err)
	}
}

// 两层叠起来也要通：生产链路是 withAccessLog(withRejectEnvelope(mux))，
// 任一层断链都等于整条失效。
func TestStackedWrappersPassWriteDeadlineThrough(t *testing.T) {
	if err := deadlineThrough(t, func(w http.ResponseWriter, r *http.Request) http.ResponseWriter {
		outer := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		return &rejectWriter{ResponseWriter: outer, server: &Server{}, req: r}
	}); err != nil {
		t.Errorf("SetWriteDeadline 穿不过两层包装：%v", err)
	}
}

// 源码级守卫：以后新增的包装类型也必须带 Unwrap。
//
// 只测现有两个类型不够——这条纪律的代价是「忘了写也一切正常」，
// 而忘写的人正是下一个加包装的人。
func TestEveryResponseWriterWrapperDeclaresUnwrap(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("ParseDir: %v", err)
	}

	wrappers := map[string]bool{}  // 嵌了 http.ResponseWriter 的类型
	hasUnwrap := map[string]bool{} // 声明了 Unwrap 方法的类型

	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.TypeSpec:
					st, ok := node.Type.(*ast.StructType)
					if !ok {
						return true
					}
					for _, f := range st.Fields.List {
						if len(f.Names) != 0 {
							continue
						}
						if sel, ok := f.Type.(*ast.SelectorExpr); ok {
							if id, ok := sel.X.(*ast.Ident); ok &&
								id.Name == "http" && sel.Sel.Name == "ResponseWriter" {
								wrappers[node.Name.Name] = true
							}
						}
					}
				case *ast.FuncDecl:
					if node.Name.Name != "Unwrap" || node.Recv == nil ||
						len(node.Recv.List) == 0 {
						return true
					}
					hasUnwrap[receiverName(node.Recv.List[0].Type)] = true
				}
				return true
			})
		}
	}

	if len(wrappers) == 0 {
		t.Fatal("一个包装类型都没找到，守卫本身失效了")
	}
	for name := range wrappers {
		if !hasUnwrap[name] {
			t.Errorf("%s 嵌了 http.ResponseWriter 却没有 Unwrap 方法，"+
				"数据面推的写 deadline 会在这一层被静默吞掉", name)
		}
	}
}

func receiverName(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if id, ok := expr.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}
