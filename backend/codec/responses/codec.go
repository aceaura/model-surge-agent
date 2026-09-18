// Package responses 实现 OpenAI Responses 协议的入站与出站编解码。
//
// 与 IR 的落差有两处。请求侧：本协议把消息、工具调用、工具结果、推理都拍平成
// input 数组里的独立条目，而 IR 把它们表达为消息内的块，所以两个方向都是
// 一对多的展开与合并。流式侧：增量用 (output_index, content_index) 两级定位，
// 入站要把它折成 IR 的一级块索引，出站要为每个条目补齐 added/done 成对帧
// 并在终止帧带上完整的 response 对象。
package responses

import (
	"strings"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// Name 也用作 Thinking.SignatureFrom 的取值：加密的推理内容只在同族协议间透传。
const Name = codec.ProtocolResponses

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

// Caps 里 ThinkingSig 为真但语义与 Anthropic 不同：本协议的签名是
// encrypted_content。StopSequences 为假——本协议没有停止序列字段。
func (outboundCodec) Caps() codec.Capabilities {
	return codec.Capabilities{
		Thinking:    true,
		ThinkingSig: true,
		Tools:       true,
		Images:      true,
		// 图片走 input_image，wav/mp3 走 input_audio，其余走 input_file。
		MediaTypes: []string{
			"image/png", "image/jpeg", "image/gif", "image/webp",
			"audio/wav", "audio/mpeg",
			"application/pdf", "text/plain", "text/csv", "application/json",
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

// Endpoint 的 stream 参数不影响路径：流式由请求体的 stream 字段决定。
func (outboundCodec) Endpoint(baseURL, _ string, _ bool) (string, map[string]string) {
	return strings.TrimRight(baseURL, "/") + "/responses", nil
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
