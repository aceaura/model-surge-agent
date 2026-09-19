package pipeline

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/url"
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

// redacted 是净化掉的 URL 的占位文本。
const redacted = "<url redacted>"

// sanitizeTransportError 把出站请求失败渲染成不含 URL 的一句话。
//
// 必须净化：*url.Error 的 Error() 内嵌完整请求 URL 含 query，而 BaseURL 由
// 调度层下发，一些部署把 key 放在 query 里。这句话随后进 error_message 列、
// Redis 实时环、管理面与**客户端可见的错误体**四个出口。同一个理由下
// AttemptRecord 刻意不记 BaseURL，传输错误这条路不能把它放回来。
//
// 归因必须留住：超时、连接被拒、DNS 失败三者的处置完全不同，只回一句
// 「连接失败」等于把这一轮排查推给抓包。归因全在 *url.Error 的内层，
// 剥掉外层刚好既去 URL 又留归因。
func sanitizeTransportError(err error) string {
	if err == nil {
		return ""
	}
	// 先精确剥已知含 URL 的那一层，而不是直接上文本扫描：这一层还带着
	// op（"Post "），扫描去不掉它。
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		err = urlErr.Err
	}
	msg := redactURLs(err.Error())
	if msg == "" {
		// 不允许空 message：它在客户端那边表现成「未知错误」，
		// 比一个笼统但确定的说法更难查。
		return "upstream request failed"
	}
	return msg
}

// redactURLs 把文本里的 URL 换成占位符。
//
// 这是兜底而非主手段：上游库换错误类型、或自己把 URL 拼进消息时，
// 剥 *url.Error 那一步就不生效了。只认 "://" 这个记号，不做内容黑名单——
// 后者会误伤正常内容且给人虚假的安全感。
func redactURLs(s string) string {
	for {
		i := strings.Index(s, "://")
		if i < 0 {
			return s
		}
		// 向前吃掉 scheme：scheme 只含字母、数字、+、-、.
		start := i
		for start > 0 && isSchemeByte(s[start-1]) {
			start--
		}
		// 向后吃到分隔符为止。引号与空白之外不切：URL 里的 / ? & = 都要一起去掉，
		// 留下半截 query 等于没净化。
		end := i + len("://")
		for end < len(s) && !isURLEndByte(s[end]) {
			end++
		}
		s = s[:start] + redacted + s[end:]
	}
}

func isSchemeByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '+', c == '-', c == '.':
		return true
	}
	return false
}

// isURLEndByte 判断一个字节是否终止 URL。
//
// 逗号与分号在 URL 里合法，但错误文本几乎总用它们分隔子句
// （`dial tcp 1.2.3.4:443: connect: connection refused`），
// 不切会把后面的归因一起吞掉。
func isURLEndByte(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '"', '\'', '`', ',', ';', '<', '>':
		return true
	}
	return false
}
