package pipeline

import (
	"compress/flate"
	"compress/gzip"
	"io"
	"net/http"
	"strings"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// decodeBody 按响应的 Content-Encoding 包一层解压 reader。
//
// 只有「标准库没解」的响应才会走到这里有活干：Transport 透明解压之后会把
// Content-Encoding 从响应头里删掉，所以这个头还在，就是标准库没解的可靠信号。
// 它没解的原因通常是有人显式设过 Accept-Encoding（见 outboundheaders.go），
// 也可能是上游回了我们没请求过的编码。
//
// 流式解压而不是读全再解：SSE 体是无界的，读全等于把逐字输出退化成
// 一整份，而这一层恰好在流式路径上。
//
// 返回的 io.Reader 不是 io.ReadCloser：底层 resp.Body 的关闭仍由
// upstream.Close 负责。gzip.Reader 的 Close 只校验尾部校验和，
// 流被中途掐断时那个错误没有诊断价值。
func decodeBody(header http.Header, body io.Reader) (io.Reader, *ir.Error) {
	enc := strings.TrimSpace(header.Get("Content-Encoding"))
	if enc == "" {
		return body, nil
	}
	// 多重编码（如 `gzip, br`）直接报错而不是尝试逐层剥：剥的顺序、
	// 每层是否受支持都要判，而这个形态在真实上游里没见过。
	// 报错让它一次暴露，比默默剥错一层喂进切帧器强。
	if strings.Contains(enc, ",") {
		return nil, ir.NewError(ir.ErrUpstream, 0, "",
			"upstream used multiple content encodings ("+enc+") which this service cannot decode")
	}
	switch strings.ToLower(enc) {
	case "identity":
		// 显式的「没压」。RFC 允许这个值，当成未知编码报错是错的。
		return body, nil
	case "gzip", "x-gzip":
		// NewReader 立刻读并校验 gzip 头，所以坏字节在这里就暴露，
		// 而不是拖到切帧器那边表现成「一帧都没解出来」。
		zr, err := gzip.NewReader(body)
		if err != nil {
			// 刻意不带 err 之外的任何内容：解压失败时手里的字节是压缩流的
			// 片段，拼进 message 可能把上游响应体的内容泄进客户端可见的错误里。
			return nil, ir.NewError(ir.ErrUpstream, 0, "",
				"upstream gzip body could not be decoded: "+err.Error())
		}
		return zr, nil
	case "deflate":
		// 裸 deflate 而不是 zlib：这个头名在实践中两种形态都有，
		// flate.NewReader 不做头校验，错的那种会在首次 Read 时报错，
		// 那条错误由切帧器的 Err() 带出来，归因仍落在上游。
		return flate.NewReader(body), nil
	default:
		// br 与 zstd 落在这里。明确报错而不是硬塞给切帧器：
		// 后者的症状是「HTTP 200 却一个事件都没解出来」，
		// 从那个症状反推到编码问题要花很久。
		return nil, ir.NewError(ir.ErrUpstream, 0, "",
			"upstream used an unsupported content encoding ("+enc+")")
	}
}
