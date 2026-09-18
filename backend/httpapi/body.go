package httpapi

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// defaultMaxBody 是请求体上限。上下文塞满的请求确实很大，
// 但没有上限意味着一个坏客户端就能把内存吃光。
const defaultMaxBody = 64 << 20

// utf8BOM 是一些 Windows 上的客户端会加在 JSON 前面的字节序标记。
// encoding/json 不接受它，带着它解码只会报一个指不出原因的语法错误。
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// readBody 读出请求体，必要时解压。
//
// 返回 *ir.Error 而不是 error：错误的种类在这里就已确定（超限是
// context_exceeded、其余是 invalid_request），交给调用方再统一兜一道
// 会把 413 打回 400。
func (s *Server) readBody(w http.ResponseWriter, r *http.Request) ([]byte, *ir.Error) {
	limit := s.MaxBodyBytes
	if limit <= 0 {
		limit = defaultMaxBody
	}
	defer r.Body.Close()

	if err := checkMediaType(r.Header.Get("Content-Type")); err != nil {
		return nil, err
	}

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		// 超限要回 413 而非 400：codec.KindForStatus 把上游的 413 归为
		// context_exceeded，本服务自己产生同类错误时走同一套语义，
		// 客户端才能据此判断该裁剪输入，而不是去逐个字段检查参数。
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, oversize(limit)
		}
		return nil, ir.NewError(ir.ErrInvalidRequest, 0, "", fmt.Sprintf("read body: %v", err))
	}

	raw, decErr := decompress(raw, r.Header.Get("Content-Encoding"), limit)
	if decErr != nil {
		return nil, decErr
	}

	raw = bytes.TrimPrefix(raw, utf8BOM)
	if len(bytes.TrimSpace(raw)) == 0 {
		// 明说请求体为空，而不是透出 json 的 "unexpected end of JSON input"：
		// 后者读的人分不清是自己没发 body 还是 body 被中间层吃了。
		return nil, ir.NewError(ir.ErrInvalidRequest, 0, "", "request body is empty")
	}
	return raw, nil
}

// oversize 是请求体超限的错误，消息里带上限的字节数。
//
// 显式带 StatusCode 413：StatusForKind 把 context_exceeded 映射成 400，
// 那是为「上游回 400 说上下文超限」服务的，这里不能跟着回 400。
// 带上限值是为了让客户端能自己分片，不必靠二分试出上限在哪。
func oversize(limit int64) *ir.Error {
	return ir.NewError(ir.ErrContextExceeded, http.StatusRequestEntityTooLarge, "",
		fmt.Sprintf("request body exceeds the %d byte limit", limit))
}

// rejectedMediaTypes 是明确不接受的请求体类型。
//
// 只列表单这两种，不做「必须是 application/json」的白名单：真实客户端
// 带的 Content-Type 五花八门（缺头、text/plain、带自家 +json 后缀），
// 按白名单挡会把一堆本能正常解码的请求挡在门外。
//
// 表单类必须挡：浏览器的 fetch 默认发 x-www-form-urlencoded，body 是
// urlencode 过的键值对。让它流到 json.Unmarshal 只会得到一句语法错误，
// 而真正的问题是「这个端点不吃表单」，前者读的人看不出后者。
var rejectedMediaTypes = map[string]bool{
	"multipart/form-data":               true,
	"application/x-www-form-urlencoded": true,
}

// checkMediaType 在读体之前挡掉不受支持的请求体类型。
//
// 头缺失或解不动时放过：缺头的客户端很多，而它们发的确实是 JSON。
//
// 显式带 415：StatusForKind 只认几档常见的，invalid_request 归 400，
// 不带就会把「类型不对」报成「参数不对」。
func checkMediaType(contentType string) *ir.Error {
	if strings.TrimSpace(contentType) == "" {
		return nil
	}
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil
	}
	if !rejectedMediaTypes[strings.ToLower(mt)] {
		return nil
	}
	return ir.NewError(ir.ErrInvalidRequest, http.StatusUnsupportedMediaType, "content-type",
		fmt.Sprintf("content-type %q is not supported, this endpoint takes a JSON body", mt))
}

// supportedEncodings 是本服务能解的传输编码。
//
// 只用标准库能解的两种。zstd 要引第三方依赖，而客户端默认不会用它——
// 三个参考实现里也只有一家支持。
var supportedEncodings = []string{"gzip", "deflate"}

// decompress 按 Content-Encoding 解压请求体。
//
// 解压后必须再限一次长度：只限压缩前等于没限，几百 KB 的 gzip 能解出
// 几百 MB，而内存是按解压后的大小吃掉的。
func decompress(raw []byte, encoding string, limit int64) ([]byte, *ir.Error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return raw, nil
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, corrupt("gzip", err)
		}
		defer zr.Close()
		return readLimited(zr, limit, "gzip")
	case "deflate":
		zr := flate.NewReader(bytes.NewReader(raw))
		defer zr.Close()
		return readLimited(zr, limit, "deflate")
	default:
		// 不静默当未压缩处理：那样客户端会收到一条 JSON 语法错误，
		// 比直接说「不支持这个编码」难查得多。
		return nil, ir.NewError(ir.ErrInvalidRequest, 0, "content-encoding",
			fmt.Sprintf("unsupported content-encoding %q, this service accepts %s",
				encoding, strings.Join(supportedEncodings, ", ")))
	}
}

// readLimited 读解压流并在超过上限时回 413。
//
// 多读一个字节才能区分「正好等于上限」与「超过上限」。
func readLimited(r io.Reader, limit int64, encoding string) ([]byte, *ir.Error) {
	out, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, corrupt(encoding, err)
	}
	if int64(len(out)) > limit {
		return nil, oversize(limit)
	}
	return out, nil
}

// corrupt 是解压失败的错误。
//
// 回 400 而不是 500：压缩流损坏是客户端发来的数据有问题，
// 记成本服务出错会让排查从一开始就走错方向。
func corrupt(encoding string, err error) *ir.Error {
	return ir.NewError(ir.ErrInvalidRequest, 0, "content-encoding",
		fmt.Sprintf("cannot decompress %s body: %v", encoding, err))
}
