package pipeline_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/pipeline"
)

// 本文件守的是死连接探测：连接看着活、实际已经不通。
//
// 只断言 Transport.HTTP2 != nil 证明的是「字段设了」，而不是「死连接会被
// 检出」——这两件事之间隔着一整个 h2 实现。所以这里真造一条死连接。

// blackhole 是一个可切换的 TCP 中继。切黑洞后静默丢弃双向字节，
// 既不关连接也不发 FIN/RST——这才是 NAT 表项超时的真实形态：
// 连接看着活，实际什么都过不去，对端连 PING 也回不来。
//
// 用「中继 + 丢字节」而不是「服务端不回响应」：后者的 h2 层仍在正常应答
// PING，探测永远探不出问题，测出来的会是一个假的通过。
type blackhole struct {
	on atomic.Bool
}

func (b *blackhole) start(t *testing.T, dst string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			u, err := net.Dial("tcp", dst)
			if err != nil {
				_ = c.Close()
				continue
			}
			go b.pipe(c, u)
			go b.pipe(u, c)
		}
	}()
	return "http://" + ln.Addr().String()
}

func (b *blackhole) pipe(from, to net.Conn) {
	buf := make([]byte, 32<<10)
	for {
		n, rerr := from.Read(buf)
		if n > 0 && !b.on.Load() {
			if _, werr := to.Write(buf[:n]); werr != nil {
				return
			}
		}
		if rerr != nil {
			return
		}
	}
}

// h2Backend 起一个明文 h2 后端。用明文而非 TLS：这一层要验的是 h2 的
// 连接健康探测，TLS 只会给测试加一份自签证书的噪音。
func h2Backend(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{
		Protocols: &http.Protocols{},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = w.Write([]byte("ok"))
		}),
	}
	srv.Protocols.SetUnencryptedHTTP2(true)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

// h2Client 把 NewHTTPClient 造出的客户端改成走明文 h2。
//
// 只改协议协商，ping 配置仍来自 NewHTTPClient——被测的正是它。
func h2Client(t *testing.T, opts pipeline.TransportOptions) *http.Client {
	t.Helper()
	c := pipeline.NewHTTPClient(opts)
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport 类型 = %T", c.Transport)
	}
	tr.Protocols = &http.Protocols{}
	tr.Protocols.SetUnencryptedHTTP2(true)
	// 被动超时设成不设限：不然量到的可能是它先触发，而不是 ping 的效果。
	tr.ResponseHeaderTimeout = 0
	return c
}

// post 发一次请求，返回耗时与错误。
func post(ctx context.Context, cl *http.Client, url string) (time.Duration, error) {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/x",
		bytes.NewReader([]byte(`{"a":1}`)))
	if err != nil {
		return 0, err
	}
	resp, err := cl.Do(req)
	if err != nil {
		return time.Since(start), err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return time.Since(start), nil
}

// 死连接必须在几秒内被探出来。
//
// ping 配成 1s/1s 而不是默认的 15s/15s：默认值下最坏检出要 30s，
// 一个测试跑半分钟不可接受。默认值本身由下面的配置矩阵守。
func TestPingDetectsDeadConnection(t *testing.T) {
	var hole blackhole
	url := hole.start(t, h2Backend(t))
	cl := h2Client(t, pipeline.TransportOptions{
		H2SendPingTimeout: time.Second,
		H2PingTimeout:     time.Second,
	})

	// 第一次成功，连接进池。
	if _, err := post(context.Background(), cl, url); err != nil {
		t.Fatalf("首次请求就失败了，测试前提不成立: %v", err)
	}

	hole.on.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	elapsed, err := post(ctx, cl, url)

	if err == nil {
		t.Fatal("撞上死连接却成功了，探测没起作用")
	}
	if ctx.Err() != nil {
		t.Fatalf("死连接直到 %v 的 ctx 超时才失败，探测没起作用："+
			"没有主动探测就只能等被动超时，那对一次尝试是整个重试预算", elapsed)
	}
	// 最坏是两个超时之和，留一倍余量。
	if elapsed > 5*time.Second {
		t.Errorf("检出耗时 %v 超出两个 ping 超时之和的合理范围", elapsed)
	}
}

// 对照组：不配 ping 时同样的死连接在同样的时限内探不出来。
//
// 没有这一格，上面那个测试无法证明是 ping 起的作用——也可能是内核或 h2
// 自己就会在 5s 内报错。
func TestWithoutPingDeadConnectionHangs(t *testing.T) {
	var hole blackhole
	url := hole.start(t, h2Backend(t))
	// 负值表示显式关闭探测。
	cl := h2Client(t, pipeline.TransportOptions{
		H2SendPingTimeout: -1,
		H2PingTimeout:     -1,
	})

	if _, err := post(context.Background(), cl, url); err != nil {
		t.Fatalf("首次请求就失败了，测试前提不成立: %v", err)
	}

	hole.on.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := post(ctx, cl, url)

	if err == nil {
		t.Fatal("切了黑洞却成功了，黑洞没生效，上面那个测试的结论不成立")
	}
	if ctx.Err() == nil {
		t.Fatalf("不配 ping 也在 5s 内失败了（%v），"+
			"说明上面那个测试测到的不是 ping 的效果", err)
	}
}

// 已开始的流上探出死连接时，错误从读流冒出来而不是 Do。
//
// 这一格决定归因能不能用：committed 之后换目标会让客户端看到两段拼接的
// 回答，所以那里刻意不做连接层归因（需求 2.6）。没有这个测试，
// 「为什么不在读流侧也归因」就只是一句注释。
func TestDeadConnectionDuringStreamSurfacesOnRead(t *testing.T) {
	var hole blackhole
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{
		Protocols: &http.Protocols{},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl, ok := w.(http.Flusher)
			if !ok {
				return
			}
			for range 200 {
				_, _ = w.Write([]byte("data: {\"i\":1}\n\n"))
				fl.Flush()
				time.Sleep(50 * time.Millisecond)
			}
		}),
	}
	srv.Protocols.SetUnencryptedHTTP2(true)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	url := hole.start(t, ln.Addr().String())
	cl := h2Client(t, pipeline.TransportOptions{
		H2SendPingTimeout: time.Second,
		H2PingTimeout:     time.Second,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/x",
		bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("Do 应当成功——流已建起来了: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	buf := make([]byte, 256)
	read := 0
	for {
		n, rerr := resp.Body.Read(buf)
		read += n
		if read > 0 && !hole.on.Load() {
			hole.on.Store(true)
		}
		if rerr != nil {
			if ctx.Err() != nil {
				t.Fatalf("读流直到 ctx 超时才终止，探测在流读路径上没起作用")
			}
			// 断言错误形态：这正是 isTransportError 用文本匹配的那一种。
			if !strings.Contains(rerr.Error(), "connection lost") {
				t.Fatalf("读流终止的错误 = %v，"+
					"isTransportError 的文本匹配是按 connection lost 写的", rerr)
			}
			return
		}
	}
}
