package pipeline

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
)

// h2ConnLost 是 h2 探测到连接失联时的错误文本。
//
// 唯一一处字符串判定。标准库 vendored 的 http2 把这一族错误做成未导出的
// errors.New 值，errors.Is 没有可比的目标。本机探针实测：给 Transport.HTTP2
// 配上 ping 之后，撞上静默黑洞的请求就是以这个文本失败的（不配则一直挂着）。
const h2ConnLost = "http2: client connection lost"

// isTransportError 判断一个请求失败是否发生在出站连接层。
//
// 连接层故障里上游可能完全健康——坏的是我们池里那条连接，所以它不该计入
// 这个目标的失败。判定尽量走类型，不走文本：上游的错误文案会变，
// 而 errors.Is/As 的目标不会。
func isTransportError(err error) bool {
	if err == nil {
		return false
	}

	// ctx 原因必须最先排除。客户端取消会包成 *url.Error 裹 context.Canceled，
	// 而下面的 net.Error 分支会把它一并认下——那样运维看到的「连接故障占比」
	// 全是客户端按停止的次数。
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	// 证书校验失败刻意不算：换条连接、换个目标都会同样失败，
	// 问题在证书或系统信任库。算进来会让一个配置错误表现成
	// 「有个目标一直报连接故障但从不冷却」，比记成目标失败更难查。
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return false
	}

	// 对端在 TLS 层发来的不是合法记录头，典型成因是中间设备截断或连接串了，
	// 换条连接有意义——与证书校验失败相反。
	var recErr *tls.RecordHeaderError
	if errors.As(err, &recErr) {
		return true
	}

	// 拨号、读写在 socket 层失败。
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}

	// 这几个通常被 *net.OpError 包着，但在部分路径上会裸出现。
	for _, errno := range []syscall.Errno{
		syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.ECONNABORTED, syscall.EPIPE,
	} {
		if errors.Is(err, errno) {
			return true
		}
	}

	// 连接在响应中途断掉。注意不认裸 io.EOF：干净的 EOF 是正常的流结束，
	// 标准库对复用连接上的 EOF 还会自己换连接重放（本机探针已实测）。
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}

	return strings.Contains(err.Error(), h2ConnLost)
}
