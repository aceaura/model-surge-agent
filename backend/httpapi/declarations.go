package httpapi

import (
	"net/http"
	"strings"

	"github.com/aceaura/model-surge-agent/backend/codec"
)

// readDeclarations 读客户端随请求发来的协议声明。
//
// 只读这两个头，不做全量透传：cc-switch 那种「原样复制整个头集合」是为了
// 保住头顺序做指纹伪装，代价是客户端能影响出站请求的任意维度——而凭据头
// 就在同一个集合里。
func readDeclarations(r *http.Request) codec.Declarations {
	return codec.Declarations{
		APIVersion: strings.TrimSpace(r.Header.Get("anthropic-version")),
		// 用 Values 而非 Get：这是列表值头，客户端可以分多行发，
		// 只取第一行会静默丢掉其余声明。
		Betas: codec.ParseBetas(r.Header.Values("anthropic-beta")),
	}
}
