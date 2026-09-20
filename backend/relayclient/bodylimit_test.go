package relayclient_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/relayclient"
)

// maxResponseBytes 在生产代码里是常量，这里重复一份而不是导出它：
// 导出会给调用方一个「上限可以变」的暗示，而它刻意不可配。
// 两处漂移会被下面这两格（上限 ±1）立刻打出来。
const limit = 8 << 20

// 超限必须明确失败。无上限时一个失控的 relay 响应会把进程内存吃光，
// 而数据面的同类读取早就有 32MiB 上限——控制面是唯一漏的那处。
func TestOversizedResponseFailsWithAttribution(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(bytes.Repeat([]byte("a"), limit+1))
	}))
	defer srv.Close()

	c := relayclient.NewWithOptions(srv.URL, "k", relayclient.Options{})
	_, err := c.Models(context.Background())
	if err == nil {
		t.Fatal("超限响应却成功返回了")
	}
	re, ok := err.(*relayclient.Error)
	if !ok {
		t.Fatalf("错误类型 %T，想要 *relayclient.Error", err)
	}
	if !strings.Contains(re.Message, "exceeds") {
		t.Errorf("消息 %q 没指明超限——排障者看不出根因", re.Message)
	}
	// 对端行为异常，重试不会让响应变小。标成可重试会让调用方白跑几轮。
	if re.Retryable {
		t.Error("超限被标成可重试")
	}
}

// 恰好等于上限要正常解码。少一个字节就失败等于上限实际是 limit-1，
// 而这类差一错误只在临界响应上暴露，平时看不出来。
func TestResponseExactlyAtLimitDecodes(t *testing.T) {
	// 造一份正好 limit 字节的合法 JSON：用一个长模型名把长度填满。
	want := relayclient.ModelsResponse{Models: []relayclient.UserModelSummary{
		{Name: "x", Collection: "c", Enabled: true},
	}}
	body, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	pad := limit - len(body)
	if pad < 0 {
		t.Fatalf("基准 JSON 已经 %d 字节，超过上限", len(body))
	}
	want.Models[0].Name = "x" + strings.Repeat("y", pad)
	body, err = json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(body) != limit {
		t.Fatalf("造出来 %d 字节，想要正好 %d", len(body), limit)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	c := relayclient.NewWithOptions(srv.URL, "k", relayclient.Options{})
	got, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("恰好等于上限却失败了：%v", err)
	}
	if len(got) != 1 || got[0].Name != want.Models[0].Name {
		t.Errorf("解出来 %d 项，名字对不上", len(got))
	}
}

// 错误状态码带着失控的大体时，报的必须是超限而不是状态码。
// 顺序反了的话运维看到的是「relay returned 500: aaaa…」的 256 字节 snippet，
// 而真正的异常——响应体失控——没有任何暴露面。
func TestOversizedErrorBodyReportsLimitNotStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write(bytes.Repeat([]byte("<html>"), (limit+1)/6+1))
	}))
	defer srv.Close()

	c := relayclient.NewWithOptions(srv.URL, "k", relayclient.Options{})
	_, err := c.Models(context.Background())
	if err == nil {
		t.Fatal("超限的 500 却成功返回了")
	}
	re := err.(*relayclient.Error)
	if !strings.Contains(re.Message, "exceeds") {
		t.Errorf("消息 %q 讲的是状态码而不是超限", re.Message)
	}
	if strings.Contains(re.Message, "<html>") {
		t.Errorf("失控的正文被拼进了消息：%.80q", re.Message)
	}
}

// 上限必须挡在读取处，不能「先全读完再判长度」。
//
// 后者的错误消息与前者一模一样，于是靠消息断言的测试分不出来——而真正要防的
// 恰恰是把失控的响应读进内存这件事本身。这里从对端侧量：客户端只读到上限就
// 停并关连接，于是服务端继续写会失败；读全了则服务端能把全部字节都写完。
func TestLimitIsEnforcedWhileReadingNotAfter(t *testing.T) {
	const chunk = 1 << 20
	const chunks = 4 * limit / chunk // 4 倍上限
	var wrote int64
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		buf := bytes.Repeat([]byte("a"), chunk)
		for i := 0; i < chunks; i++ {
			n, err := w.Write(buf)
			atomic.AddInt64(&wrote, int64(n))
			if err != nil {
				return
			}
			// Flush 让字节真的上路：攒在缓冲里的话客户端还没开始读就写完了，
			// 量不出差别。
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer srv.Close()

	c := relayclient.NewWithOptions(srv.URL, "k", relayclient.Options{})
	if _, err := c.Models(context.Background()); err == nil {
		t.Fatal("4 倍上限的响应却成功返回了")
	}
	<-done

	got := atomic.LoadInt64(&wrote)
	// 留一倍余量：TCP 与 http 各层都有缓冲，客户端停读之后服务端还能再写一截。
	if got > 2*int64(limit) {
		t.Errorf("对端写出了 %d 字节（上限 %d）——客户端把整份都读完了，"+
			"上限成了读完之后的事后检查", got, limit)
	}
}

// 上限之内的错误体照旧走信封解析这条路：加上限不能把既有的错误归因弄坏。
func TestNormalErrorBodyStillDecodesEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":{"code":"target_unavailable","message":"no candidates","retryable":false}}`))
	}))
	defer srv.Close()

	c := relayclient.NewWithOptions(srv.URL, "k", relayclient.Options{})
	_, err := c.Models(context.Background())
	re, ok := err.(*relayclient.Error)
	if !ok {
		t.Fatalf("错误类型 %T", err)
	}
	if re.Code != relayclient.CodeTargetUnavailable {
		t.Errorf("码 = %q，想要 %q", re.Code, relayclient.CodeTargetUnavailable)
	}
	if re.Message != "no candidates" {
		t.Errorf("消息 = %q，信封没被采纳", re.Message)
	}
}
