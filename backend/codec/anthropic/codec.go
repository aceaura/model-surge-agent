// Package anthropic 实现 Anthropic Messages 协议的入站与出站编解码。
//
// IR 的事件词汇取自本协议的流式模型（块生命周期 + 增量类型 + 独立 usage 帧），
// 因此这个包是 IR 的参照实现：另外三个协议的 codec 以「能否无损投影到这里」
// 为正确性判据。
package anthropic

import (
	"strings"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// Name 也用作 Thinking.SignatureFrom 的取值：签名只在同族协议间透传。
const Name = codec.ProtocolAnthropic

const apiVersion = "2023-06-01"

type inboundCodec struct{}

func (inboundCodec) Name() string { return Name }

func (inboundCodec) DecodeRequest(body []byte) (*ir.Request, error) {
	return DecodeRequest(body)
}

func (inboundCodec) NewStreamEncoder() codec.StreamEncoder { return newStreamEncoder() }

func (inboundCodec) EncodeResponse(resp *ir.Response) ([]byte, error) {
	return EncodeResponse(resp)
}

func (inboundCodec) RenderError(err *ir.Error) (int, []byte) { return RenderError(err) }

func (inboundCodec) RenderStreamError(err *ir.Error) [][]byte { return RenderStreamError(err) }

type outboundCodec struct{}

func (outboundCodec) Name() string { return Name }

func (outboundCodec) Caps() codec.Capabilities {
	return codec.Capabilities{
		Thinking:      true,
		ThinkingSig:   true,
		Tools:         true,
		Images:        true,
		CacheControl:  true,
		TopK:          true,
		StopSequences: true,
		// 本协议只读图片与 PDF；音频与其他附件在编码时降级为文本。
		MediaTypes: []string{
			"image/png", "image/jpeg", "image/gif", "image/webp",
			"application/pdf",
		},
	}
}

// EncodeRequest 是 EncodeRequestLossy 丢弃诊断的包装：两条路径共用同一编码，
// 请求体逐字节相同，否则提示缓存前缀会因诊断开关而漂移。
func (c outboundCodec) EncodeRequest(req *ir.Request) ([]byte, error) {
	body, _, err := c.EncodeRequestLossy(req)
	return body, err
}

func (outboundCodec) EncodeRequestLossy(req *ir.Request) ([]byte, []string, error) {
	body, err := EncodeRequest(req)
	if err != nil {
		return nil, nil, err
	}
	return body, codec.DescribeLossy(req, Name, outboundCodec{}.Caps()), nil
}

// Endpoint 的 stream 参数在本协议下不影响路径：流式由请求体的 stream 字段决定。
func (outboundCodec) Endpoint(baseURL, _ string, _ bool) (string, map[string]string) {
	return strings.TrimRight(baseURL, "/") + "/v1/messages",
		map[string]string{"anthropic-version": apiVersion}
}

func (outboundCodec) NewStreamDecoder() codec.StreamDecoder { return newStreamDecoder() }

func (outboundCodec) DecodeResponse(body []byte) (*ir.Response, error) { return DecodeResponse(body) }

func (outboundCodec) DecodeError(status int, body []byte) *ir.Error {
	return DecodeError(status, body)
}

func init() {
	codec.RegisterInbound(inboundCodec{})
	codec.RegisterOutbound(outboundCodec{})
}
