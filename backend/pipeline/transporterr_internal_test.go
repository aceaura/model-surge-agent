package pipeline

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"syscall"
	"testing"
)

// 本文件守连接层归因：判错方向会让运维看到的故障分布整体失真。
//
// 判 false 的那一侧与判 true 的一侧同等重要——把客户端取消认成连接故障，
// 运维看到的「连接故障占比」会全是客户端按停止的次数。

func TestIsTransportErrorTrueSide(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"socket 层失败", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("no route to host")}},
		{"连接被重置", syscall.ECONNRESET},
		{"连接被拒", syscall.ECONNREFUSED},
		{"连接被中止", syscall.ECONNABORTED},
		{"写入已关闭的管道", syscall.EPIPE},
		{"响应中途断掉", io.ErrUnexpectedEOF},
		{"TLS 记录头非法", &tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}},
		{"h2 连接失联", errors.New("http2: client connection lost")},

		// 真实形态：标准库把 Do 的失败裹在 *url.Error 里。
		{"url.Error 裹 socket 错误", &url.Error{Op: "Post", URL: "https://x/y",
			Err: &net.OpError{Op: "read", Err: syscall.ECONNRESET}}},
		{"url.Error 裹 h2 失联", &url.Error{Op: "Post", URL: "https://x/y",
			Err: errors.New("http2: client connection lost")}},
		{"多层包裹", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", syscall.ECONNRESET))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !isTransportError(c.err) {
				t.Fatalf("%v 应判为连接层故障，否则一条坏连接会被记成目标的失败", c.err)
			}
		})
	}
}

func TestIsTransportErrorFalseSide(t *testing.T) {
	cases := []struct {
		name string
		err  error
		why  string
	}{
		{"nil", nil, "没有错误"},
		{"客户端取消", context.Canceled,
			"客户端取消已有 ErrCanceled，混进来会让连接故障占比全是假的"},
		{"客户端取消裹在 url.Error 里", &url.Error{Op: "Post", URL: "https://x/y",
			Err: context.Canceled}, "这是客户端取消的真实形态"},
		{"ctx 超时", context.DeadlineExceeded, "超时有自己的归类"},
		{"ctx 超时裹在 url.Error 里", &url.Error{Op: "Post", Err: context.DeadlineExceeded}, "同上"},
		{"证书校验失败", &tls.CertificateVerificationError{
			UnverifiedCertificates: []*x509.Certificate{}, Err: errors.New("x509: certificate signed by unknown authority")},
			"换连接换目标都会同样失败，问题在证书或信任库"},
		{"裸 EOF", io.EOF,
			"干净的 EOF 是正常流结束，标准库对复用连接上的 EOF 还会自己换连接重放"},
		{"普通错误", errors.New("something went wrong"), "不是连接层"},
		{"上游 5xx 的文本", errors.New("upstream returned 503"), "有响应就不是连接层"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if isTransportError(c.err) {
				t.Fatalf("%v 不该判为连接层故障：%s", c.err, c.why)
			}
		})
	}
}

// 排除 ctx 的那一步必须在类型判定之前。
//
// 把 context.Canceled 包进 *net.OpError 是真实形态（拨号途中 ctx 被取消），
// 顺序反了就会认成连接故障。
func TestCtxCauseBeatsTypeMatch(t *testing.T) {
	err := &url.Error{Op: "Post", Err: &net.OpError{Op: "dial", Err: context.Canceled}}
	if isTransportError(err) {
		t.Fatal("ctx 取消包在 net.OpError 里时仍应判 false：" +
			"排除 ctx 原因必须先于类型判定，否则客户端取消会被算成我们的连接坏了")
	}
}
