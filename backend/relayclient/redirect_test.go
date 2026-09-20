package relayclient_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/relayclient"
)

// 控制面不得跟随重定向。默认策略下本机探针实测：一个 302 会把带 body 的
// POST /v1/dispatch 改写成无体 GET 打向重定向目标，同 host 时 Authorization
// 原样带过去，而调用方拿到的是 200 与目标伪造的那份调度结果——整条链上没有
// 任何暴露面，数据面会照着一个陌生来源给的目标去发用户请求。
func TestRedirectIsRefusedAndTargetNeverReached(t *testing.T) {
	var hits int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Write([]byte(`{"models":[{"name":"planted"}]}`))
	}))
	defer target.Close()

	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusFound)
	}))
	defer src.Close()

	c := relayclient.NewWithOptions(src.URL, "dispatch-secret", relayclient.Options{})
	got, err := c.Models(context.Background())
	if err == nil {
		t.Fatalf("重定向被跟随了，拿到 %d 项模型", len(got))
	}
	if n := atomic.LoadInt64(&hits); n != 0 {
		t.Errorf("重定向目标被访问了 %d 次", n)
	}
	re, ok := err.(*relayclient.Error)
	if !ok {
		t.Fatalf("错误类型 %T，想要 *relayclient.Error", err)
	}
	if !strings.Contains(re.Message, "redirect") {
		t.Errorf("消息 %q 没指明是重定向", re.Message)
	}
}

// 错误消息里不得出现重定向目标：那个 URL 的 query 里可能带密钥，
// 而错误消息会进日志与流水。
func TestRedirectErrorOmitsTheTargetURL(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer target.Close()
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/models?leaked_key=s3cr3t", http.StatusFound)
	}))
	defer src.Close()

	c := relayclient.NewWithOptions(src.URL, "k", relayclient.Options{})
	_, err := c.Models(context.Background())
	if err == nil {
		t.Fatal("重定向被跟随了")
	}
	msg := err.(*relayclient.Error).Message
	if strings.Contains(msg, "leaked_key") || strings.Contains(msg, "s3cr3t") {
		t.Errorf("重定向目标的 query 进了错误消息：%q", msg)
	}
	if strings.Contains(msg, target.URL) {
		t.Errorf("重定向目标 URL 进了错误消息：%q", msg)
	}
}

// 跨 host 的重定向同样不跟随，且凭据不发往 baseURL 之外的任何 host。
// 标准库跨 host 时会自己删 Authorization（实测为空），但请求仍然成功并采纳
// 对方的响应体——「凭据没漏」不等于「结果可信」。
func TestCrossHostRedirectSendsNoCredentialAnywhere(t *testing.T) {
	var sawAuth atomic.Value
	sawAuth.Store("")
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth.Store("visited:" + r.Header.Get("Authorization"))
		w.Write([]byte(`{"models":[]}`))
	}))
	defer target.Close()
	// localhost 与 127.0.0.1 在标准库看是不同 host。
	_, port, err := net.SplitHostPort(strings.TrimPrefix(target.URL, "http://"))
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	cross := "http://localhost:" + port

	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, cross+r.URL.Path, http.StatusFound)
	}))
	defer src.Close()

	c := relayclient.NewWithOptions(src.URL, "dispatch-secret", relayclient.Options{})
	if _, err := c.Models(context.Background()); err == nil {
		t.Fatal("跨 host 重定向被跟随了")
	}
	if got := sawAuth.Load().(string); got != "" {
		t.Errorf("跨 host 目标被访问了（%s）", got)
	}
}

// 2xx 与 4xx/5xx 都不受本策略影响：拒绝重定向不能把正常路径连带掐掉。
func TestNonRedirectStatusesAreUnaffected(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{name: "200 照常解码", status: 200, body: `{"models":[]}`},
		{name: "404 照常归因", status: 404, body: `{}`, wantErr: true},
		{name: "503 照常归因", status: 503, body: `{}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := relayclient.NewWithOptions(srv.URL, "k", relayclient.Options{})
			_, err := c.Models(context.Background())
			if tc.wantErr != (err != nil) {
				t.Errorf("err = %v，wantErr = %v", err, tc.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "redirect") {
				t.Errorf("非 3xx 被归因成重定向：%v", err)
			}
		})
	}
}
