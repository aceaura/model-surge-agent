package relayclient_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/relayclient"
)

// 控制面的连接必须复用。不设 PerHost 时标准库取 2，而控制面的全部请求都打向
// 同一个 relay host——本机探针实测并发 8 两轮 16 次请求拨号 16 次、复用 0 次。
// 这是数据面每次调用都要先付一次的代价，且症状只是「慢」，没有任何报错。
func TestControlPlaneReusesConnections(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 睡一下让并发真正重叠：瞬时返回会让请求串行完成，
		// 于是连 PerHost=2 也够用，测不出差别。
		time.Sleep(20 * time.Millisecond)
		w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()

	var dials int64
	base := &net.Dialer{}
	opts := relayclient.Options{}
	c := relayclient.NewWithOptions(srv.URL, "k", opts)
	// 从客户端自己的 transport 上挂计数，而不是另造一个：要测的正是
	// NewWithOptions 配出来的那一份。
	tr, ok := relayclient.TransportOf(c)
	if !ok {
		t.Fatal("客户端的 Transport 不是 *http.Transport")
	}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		atomic.AddInt64(&dials, 1)
		return base.DialContext(ctx, network, addr)
	}

	const concurrency = 8
	const rounds = 2
	for round := 0; round < rounds; round++ {
		var wg sync.WaitGroup
		for i := 0; i < concurrency; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := c.Models(context.Background()); err != nil {
					t.Errorf("Models: %v", err)
				}
			}()
		}
		wg.Wait()
	}

	total := int64(concurrency * rounds)
	got := atomic.LoadInt64(&dials)
	if got >= total {
		t.Errorf("拨号 %d 次做了 %d 次请求，一次都没复用（PerHost 没设住）", got, total)
	}
	// 第二轮理应全部复用第一轮留下的空闲连接。
	if got > concurrency {
		t.Errorf("拨号 %d 次，超过并发数 %d——第二轮没能复用", got, concurrency)
	}
}

// 零值取内置默认，负值表示显式不设限。零值若表示「不设限」，忘配的部署会
// 静默回到无限等待，而那正是本轮要修的故障。
func TestTransportDefaultsAndExplicitOff(t *testing.T) {
	cases := []struct {
		name                 string
		opts                 relayclient.Options
		wantIdle             int
		wantPerHost          int
		wantIdleConnTimeout  time.Duration
		wantResponseHeaderTO time.Duration
		wantClientTimeout    time.Duration
	}{
		{
			name:                 "零值全取默认",
			opts:                 relayclient.Options{},
			wantIdle:             64,
			wantPerHost:          32,
			wantIdleConnTimeout:  90 * time.Second,
			wantResponseHeaderTO: 10 * time.Second,
			wantClientTimeout:    30 * time.Second,
		},
		{
			name: "显式值照用",
			opts: relayclient.Options{
				MaxIdleConns: 7, MaxIdleConnsPerHost: 3,
				IdleConnTimeout: time.Second, ResponseHeaderTimeout: 2 * time.Second,
				Timeout: 3 * time.Second,
			},
			wantIdle: 7, wantPerHost: 3,
			wantIdleConnTimeout:  time.Second,
			wantResponseHeaderTO: 2 * time.Second,
			wantClientTimeout:    3 * time.Second,
		},
		{
			name: "负值表示不设限",
			opts: relayclient.Options{
				IdleConnTimeout: -1, ResponseHeaderTimeout: -1, Timeout: -1,
			},
			wantIdle: 64, wantPerHost: 32,
			wantIdleConnTimeout:  0,
			wantResponseHeaderTO: 0,
			wantClientTimeout:    0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := relayclient.NewWithOptions("http://relay.invalid", "k", tc.opts)
			tr, ok := relayclient.TransportOf(c)
			if !ok {
				t.Fatal("Transport 不是 *http.Transport")
			}
			if tr.MaxIdleConns != tc.wantIdle {
				t.Errorf("MaxIdleConns = %d，想要 %d", tr.MaxIdleConns, tc.wantIdle)
			}
			if tr.MaxIdleConnsPerHost != tc.wantPerHost {
				t.Errorf("MaxIdleConnsPerHost = %d，想要 %d", tr.MaxIdleConnsPerHost, tc.wantPerHost)
			}
			if tr.IdleConnTimeout != tc.wantIdleConnTimeout {
				t.Errorf("IdleConnTimeout = %v，想要 %v", tr.IdleConnTimeout, tc.wantIdleConnTimeout)
			}
			if tr.ResponseHeaderTimeout != tc.wantResponseHeaderTO {
				t.Errorf("ResponseHeaderTimeout = %v，想要 %v",
					tr.ResponseHeaderTimeout, tc.wantResponseHeaderTO)
			}
			if to := relayclient.ClientTimeoutOf(c); to != tc.wantClientTimeout {
				t.Errorf("Client.Timeout = %v，想要 %v", to, tc.wantClientTimeout)
			}
		})
	}
}

// PerHost 必须显式设：标准库的默认值是 2，这条把「有人把显式设置删掉」
// 与「默认值恰好也是 2」区分开。
func TestPerHostDefaultIsNotTheStdlibDefault(t *testing.T) {
	c := relayclient.NewWithOptions("http://relay.invalid", "k", relayclient.Options{})
	tr, _ := relayclient.TransportOf(c)
	if tr.MaxIdleConnsPerHost == http.DefaultMaxIdleConnsPerHost {
		t.Errorf("PerHost = %d，与标准库默认值相同——等于没设",
			tr.MaxIdleConnsPerHost)
	}
}

// 「连上却不回响应头」必须由 ResponseHeaderTimeout 报出，而不是等总超时。
// 总超时命中时的错误文本是 "Client.Timeout or context cancellation while
// reading body"，与「调用方主动取消」同文本，排障时分不出是谁的问题。
func TestResponseHeaderTimeoutFiresBeforeTotalTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	// 接受连接后什么都不写：这正是「relay 接受了连接却永不回响应头」。
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// 不关：关掉会让客户端立刻收到 EOF，测不到超时。
			_ = conn
		}
	}()

	c := relayclient.NewWithOptions("http://"+ln.Addr().String(), "k",
		relayclient.Options{
			ResponseHeaderTimeout: 300 * time.Millisecond,
			Timeout:               10 * time.Second,
		})
	start := time.Now()
	_, err = c.Models(context.Background())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("上游不回响应头，却成功返回了")
	}
	if elapsed > 3*time.Second {
		t.Errorf("耗时 %v——等到了总超时那一档，分层没生效", elapsed)
	}
	re, ok := err.(*relayclient.Error)
	if !ok {
		t.Fatalf("错误类型 %T，想要 *relayclient.Error", err)
	}
	if !re.Retryable {
		t.Error("网络层失败该是可重试的")
	}
}

// 默认不走环境代理。控制面的 baseURL 在集群里是服务名，而这类主机名会命中
// HTTP_PROXY（只有 127.0.0.1/localhost 被内置规则排除），于是调度调用被送去
// 一个不认识它的外网代理，而症状只是「relay 不可达」。
func TestProxyOffIgnoresEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:7897")
	t.Setenv("NO_PROXY", "")
	c := relayclient.NewWithOptions("http://relay-internal:18101", "k", relayclient.Options{})
	tr, _ := relayclient.TransportOf(c)
	if tr.Proxy != nil {
		t.Error("默认策略下 Proxy 仍非 nil——环境代理会劫持控制面调用")
	}
}

// 显式要求走代理时必须真的生效，否则那些 relay 在另一网络域的部署没有出路。
func TestProxyEnvironmentHonoursEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:7897")
	t.Setenv("NO_PROXY", "")
	c := relayclient.NewWithOptions("http://relay-internal:18101", "k",
		relayclient.Options{Proxy: relayclient.ProxyEnvironment})
	tr, _ := relayclient.TransportOf(c)
	if tr.Proxy == nil {
		t.Fatal("显式要求走环境代理，Proxy 却是 nil")
	}
	u, _ := url.Parse("http://relay-internal:18101/v1/models")
	got, err := tr.Proxy(&http.Request{URL: u})
	if err != nil {
		t.Fatalf("Proxy: %v", err)
	}
	if got == nil || got.Host != "127.0.0.1:7897" {
		t.Errorf("代理取到 %v，想要 127.0.0.1:7897", got)
	}
}

// 非法策略值必须报错。静默回落 off 会让明确想走代理的人以为配好了。
func TestParseProxyModeRejectsUnknown(t *testing.T) {
	cases := []struct {
		in      string
		want    relayclient.ProxyMode
		wantErr bool
	}{
		{in: "", want: relayclient.ProxyOff},
		{in: "off", want: relayclient.ProxyOff},
		{in: "environment", want: relayclient.ProxyEnvironment},
		{in: "on", wantErr: true},
		{in: "ENVIRONMENT", wantErr: true},
		{in: "true", wantErr: true},
	}
	for _, tc := range cases {
		got, err := relayclient.ParseProxyMode(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseProxyMode(%q) 没报错，得到 %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseProxyMode(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseProxyMode(%q) = %q，想要 %q", tc.in, got, tc.want)
		}
	}
}
