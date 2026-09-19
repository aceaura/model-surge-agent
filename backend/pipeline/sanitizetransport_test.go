package pipeline

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"
	"testing"
)

// 传输错误的 message 不得含 URL：BaseURL 由调度层下发，一些部署把 key 放在
// query 里，而这句话会流到客户端可见的错误体。
func TestSanitizeStripsURLFromURLError(t *testing.T) {
	err := &url.Error{
		Op:  "Post",
		URL: "https://upstream.example.com/v1/messages?key=sk-secret-1234",
		Err: errors.New("dial tcp 1.2.3.4:443: connect: connection refused"),
	}
	got := sanitizeTransportError(err)
	if strings.Contains(got, "sk-secret-1234") {
		t.Fatalf("凭据泄漏：%q", got)
	}
	if strings.Contains(got, "upstream.example.com") {
		t.Errorf("主机名泄漏：%q", got)
	}
	if strings.Contains(got, "://") {
		t.Errorf("URL 未净化：%q", got)
	}
	// 外层必须是剥掉的，不是扫描替换的：*url.Error 的文本形如
	// `Post "URL": inner`，扫描只能拿掉 URL 本身，op 与引号会留下来，
	// 于是客户端看到 `Post <url redacted>": ...` 这种半截文本。
	if strings.Contains(got, "Post") || strings.Contains(got, `"`) {
		t.Errorf("url.Error 外层没剥掉，只做了文本替换：%q", got)
	}
}

// 归因必须留住：超时、连接被拒、DNS 失败三者的处置完全不同。
func TestSanitizeKeepsAttribution(t *testing.T) {
	cases := []struct {
		name  string
		inner string
		want  string
	}{
		{"timeout", "dial tcp 1.2.3.4:443: i/o timeout", "i/o timeout"},
		{"refused", "dial tcp 1.2.3.4:443: connect: connection refused", "connection refused"},
		{"dns", "dial tcp: lookup h.example.com: no such host", "no such host"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := &url.Error{Op: "Post", URL: "https://h/v1?key=sk-x",
				Err: errors.New(c.inner)}
			got := sanitizeTransportError(err)
			if !strings.Contains(got, c.want) {
				t.Errorf("归因丢了：got %q, want 含 %q", got, c.want)
			}
			if strings.Contains(got, "sk-x") {
				t.Errorf("凭据泄漏：%q", got)
			}
		})
	}
}

// 净化对 *url.Error 之外的形态同样生效：上游库可能换错误类型，
// 也可能自己把 URL 拼进消息。
func TestSanitizeRedactsURLInPlainError(t *testing.T) {
	err := fmt.Errorf("giving up on https://h.example.com/v1/m?key=sk-plain after 3 tries")
	got := sanitizeTransportError(err)
	if strings.Contains(got, "sk-plain") {
		t.Fatalf("凭据泄漏：%q", got)
	}
	if !strings.Contains(got, redacted) {
		t.Errorf("未留占位符：%q", got)
	}
	// 周边文字要留下：整句抹掉等于把排查线索也抹了。
	if !strings.Contains(got, "after 3 tries") {
		t.Errorf("周边细节被吞：%q", got)
	}
	if !strings.Contains(got, "giving up on") {
		t.Errorf("前半句被吞：%q", got)
	}
}

// 包在 *url.Error 里层的 URL 也要净化：两层机制缺一不可。
func TestSanitizeRedactsURLNestedInsideURLError(t *testing.T) {
	err := &url.Error{Op: "Post", URL: "https://outer/v1?key=sk-outer",
		Err: errors.New("proxy https://proxy.local:8080/?token=sk-inner refused")}
	got := sanitizeTransportError(err)
	for _, leak := range []string{"sk-outer", "sk-inner", "proxy.local"} {
		if strings.Contains(got, leak) {
			t.Errorf("%q 泄漏：%q", leak, got)
		}
	}
}

// 不含 URL 的错误原样保留，不得因为净化而损失细节。
func TestSanitizeKeepsURLFreeErrorVerbatim(t *testing.T) {
	err := errors.New("http2: client connection lost")
	if got := sanitizeTransportError(err); got != err.Error() {
		t.Errorf("被改写了：got %q, want %q", got, err.Error())
	}
}

// 多个 URL 全部净化，不止第一个。
func TestSanitizeRedactsEveryURL(t *testing.T) {
	err := errors.New("tried https://a/?k=sk-a then https://b/?k=sk-b")
	got := sanitizeTransportError(err)
	for _, leak := range []string{"sk-a", "sk-b"} {
		if strings.Contains(got, leak) {
			t.Errorf("%q 泄漏：%q", leak, got)
		}
	}
	if n := strings.Count(got, redacted); n != 2 {
		t.Errorf("占位符 = %d 个，want 2：%q", n, got)
	}
}

// message 不得为空：空 message 在客户端那边表现成「未知错误」。
//
// 用文本为空的内层错误而不是「文本恰好是一个 URL」的：后者净化后剩下占位符，
// 本来就非空，断言恒成立。剥掉 url.Error 外层之后手里只剩内层，
// 内层空则整句为空——这才是兜底真正要接住的形态。
func TestSanitizeNeverReturnsEmpty(t *testing.T) {
	err := &url.Error{Op: "Post", URL: "https://h/?k=sk-x", Err: emptyTextError{}}
	got := sanitizeTransportError(err)
	if got == "" {
		t.Fatal("返回了空 message")
	}
	if strings.Contains(got, "sk-x") {
		t.Errorf("兜底文本里带上了凭据：%q", got)
	}
}

// emptyTextError 的 Error() 返回空串。真实世界里这来自某些自定义
// transport 包装器，它们只实现接口而不给文案。
type emptyTextError struct{}

func (emptyTextError) Error() string { return "" }

func TestSanitizeNilIsEmpty(t *testing.T) {
	if got := sanitizeTransportError(nil); got != "" {
		t.Errorf("nil 应回空串，got %q", got)
	}
}

// 包裹链里的 *net.OpError 同样要能被剥到：真实的连接失败长这样。
func TestSanitizeHandlesWrappedOpError(t *testing.T) {
	op := &net.OpError{Op: "dial", Net: "tcp",
		Addr: &net.TCPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 443},
		Err:  syscall.ECONNREFUSED}
	err := &url.Error{Op: "Post", URL: "https://h/v1/m?key=sk-op", Err: op}
	got := sanitizeTransportError(err)
	if strings.Contains(got, "sk-op") {
		t.Fatalf("凭据泄漏：%q", got)
	}
	if !strings.Contains(got, "dial") {
		t.Errorf("归因丢了：%q", got)
	}
}
