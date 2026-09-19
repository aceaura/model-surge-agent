package pipeline

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func headerWith(enc string) http.Header {
	h := http.Header{}
	if enc != "" {
		h.Set("Content-Encoding", enc)
	}
	return h
}

func gzipBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func flateBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	fw, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		t.Fatalf("flate writer: %v", err)
	}
	if _, err := fw.Write([]byte(s)); err != nil {
		t.Fatalf("flate write: %v", err)
	}
	if err := fw.Close(); err != nil {
		t.Fatalf("flate close: %v", err)
	}
	return buf.Bytes()
}

func TestDecodeBodyGzip(t *testing.T) {
	r, irErr := decodeBody(headerWith("gzip"), bytes.NewReader(gzipBytes(t, "hello sse")))
	if irErr != nil {
		t.Fatalf("解压报错：%v", irErr)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("读解压流：%v", err)
	}
	if string(got) != "hello sse" {
		t.Errorf("解出来的内容 = %q", got)
	}
}

// x-gzip 是同一种编码的老写法，真实网关里出现过。
func TestDecodeBodyXGzip(t *testing.T) {
	r, irErr := decodeBody(headerWith("x-gzip"), bytes.NewReader(gzipBytes(t, "old name")))
	if irErr != nil {
		t.Fatalf("解压报错：%v", irErr)
	}
	got, _ := io.ReadAll(r)
	if string(got) != "old name" {
		t.Errorf("解出来的内容 = %q", got)
	}
}

func TestDecodeBodyDeflate(t *testing.T) {
	r, irErr := decodeBody(headerWith("deflate"), bytes.NewReader(flateBytes(t, "deflated")))
	if irErr != nil {
		t.Fatalf("解压报错：%v", irErr)
	}
	got, _ := io.ReadAll(r)
	if string(got) != "deflated" {
		t.Errorf("解出来的内容 = %q", got)
	}
}

// 头大小写不敏感：HTTP 头取值本身规范化了，但取值的大小写不是。
func TestDecodeBodyEncodingValueIsCaseInsensitive(t *testing.T) {
	r, irErr := decodeBody(headerWith("GZIP"), bytes.NewReader(gzipBytes(t, "upper")))
	if irErr != nil {
		t.Fatalf("大写取值没认出来：%v", irErr)
	}
	got, _ := io.ReadAll(r)
	if string(got) != "upper" {
		t.Errorf("解出来的内容 = %q", got)
	}
}

// 无头与 identity 都必须原样透传，且必须是同一个 reader——
// 包一层空壳虽然行为等价，但会掩盖「这条路径根本没走解压」这件事。
func TestDecodeBodyPassesThroughUncompressed(t *testing.T) {
	for _, enc := range []string{"", "identity", "  ", "IDENTITY"} {
		body := strings.NewReader("plain")
		r, irErr := decodeBody(headerWith(enc), body)
		if irErr != nil {
			t.Fatalf("enc=%q 报错了：%v", enc, irErr)
		}
		got, _ := io.ReadAll(r)
		if string(got) != "plain" {
			t.Errorf("enc=%q 内容被改了：%q", enc, got)
		}
	}
}

// br 与 zstd 必须明确报错。硬塞给切帧器的话症状是
// 「HTTP 200 却一个事件都没解出来」，从那儿反推到编码要花很久。
func TestDecodeBodyRejectsUnsupportedEncodings(t *testing.T) {
	for _, enc := range []string{"br", "zstd", "compress", "gzip2"} {
		r, irErr := decodeBody(headerWith(enc), strings.NewReader("xx"))
		if irErr == nil {
			t.Fatalf("enc=%q 没报错", enc)
		}
		if r != nil {
			t.Errorf("enc=%q 报错的同时还返回了 reader", enc)
		}
		if !strings.Contains(irErr.Message, enc) {
			t.Errorf("enc=%q 的错误没点名编码：%q", enc, irErr.Message)
		}
	}
}

// 多重编码单独一条路径：它的错误措辞与「不支持」不同，
// 因为处置方向不同（前者要上游别叠，后者要我们加解码器）。
func TestDecodeBodyRejectsMultipleEncodings(t *testing.T) {
	r, irErr := decodeBody(headerWith("gzip, br"), bytes.NewReader(gzipBytes(t, "x")))
	if irErr == nil {
		t.Fatal("多重编码没报错")
	}
	if r != nil {
		t.Error("报错的同时还返回了 reader")
	}
	if !strings.Contains(irErr.Message, "multiple") {
		t.Errorf("错误没说是多重编码：%q", irErr.Message)
	}
}

// 声明 gzip 但字节是明文：必须在这里就报错，而不是拖到切帧器那边
// 表现成「一帧都没解出来」——后者会被判成可重试，三个目标全走一遍才失败。
func TestDecodeBodyRejectsCorruptGzip(t *testing.T) {
	r, irErr := decodeBody(headerWith("gzip"), strings.NewReader("not gzip at all"))
	if irErr == nil {
		t.Fatal("坏 gzip 没在建流阶段报错")
	}
	if r != nil {
		t.Error("报错的同时还返回了 reader")
	}
}

// 解压失败的错误里不得带响应体内容：那些字节可能是上游回的任何东西，
// 而这个 message 会流到客户端可见的错误体里。
func TestCorruptGzipErrorLeaksNoBodyContent(t *testing.T) {
	const secret = "TOP-SECRET-PAYLOAD"
	_, irErr := decodeBody(headerWith("gzip"), strings.NewReader(secret))
	if irErr == nil {
		t.Fatal("坏 gzip 没报错")
	}
	if strings.Contains(irErr.Message, secret) {
		t.Errorf("错误信息里带上了响应体内容：%q", irErr.Message)
	}
}

// 解压必须是流式的：不能读全再解，否则 SSE 的逐字输出会退化成一整份。
//
// 用一个「写完一段就永久阻塞」的 reader：读全的实现会卡在这里直到超时，
// 流式的实现能立刻拿到第一段。这是行为差异，不是实现细节。
func TestDecodeBodyGzipIsStreaming(t *testing.T) {
	// Flush 而不 Close：这正是上游边生成边压缩时连接上的形态——
	// 压缩流还没结束，但已写入的内容可以解出来。刻意不用两个完整 gzip
	// 成员拼接：那种形态下 gzip.Reader 解完第一段会去读下一段的头部而阻塞，
	// 测出来的是多成员前瞻，不是流式性。
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte("first chunk")); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Flush(); err != nil {
		t.Fatalf("gzip flush: %v", err)
	}
	body := io.MultiReader(bytes.NewReader(buf.Bytes()), blockingReader{})

	r, irErr := decodeBody(headerWith("gzip"), body)
	if irErr != nil {
		t.Fatalf("解压报错：%v", irErr)
	}

	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	out := make([]byte, len("first chunk"))
	go func() {
		n, err := io.ReadFull(r, out)
		done <- result{n, err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("读第一段出错：%v", got.err)
		}
		if string(out) != "first chunk" {
			t.Errorf("第一段内容 = %q", out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("读第一段卡住了：解压不是流式的，它在等整个响应体")
	}
}

// blockingReader 永久阻塞。模拟「上游还在生成、后续字节没到」。
type blockingReader struct{}

func (blockingReader) Read([]byte) (int, error) {
	// 睡到测试结束。返回 0, nil 会让调用方忙等烧 CPU。
	time.Sleep(time.Minute)
	return 0, io.EOF
}
