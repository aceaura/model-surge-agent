package codec

import (
	"fmt"
	"maps"
	"slices"
)

// 注册表在 init 期填充，运行期只读，因此无需加锁。
var (
	inbound  = map[string]InboundCodec{}
	outbound = map[string]OutboundCodec{}
)

// RegisterInbound 注册入站 codec。重名是编程错误，直接 panic：
// 这只可能发生在 init 期，让它在启动时就炸掉而不是运行期静默覆盖。
func RegisterInbound(c InboundCodec) {
	if _, dup := inbound[c.Name()]; dup {
		panic(fmt.Sprintf("codec: inbound %q registered twice", c.Name()))
	}
	inbound[c.Name()] = c
}

func RegisterOutbound(c OutboundCodec) {
	if _, dup := outbound[c.Name()]; dup {
		panic(fmt.Sprintf("codec: outbound %q registered twice", c.Name()))
	}
	outbound[c.Name()] = c
}

func Inbound(name string) (InboundCodec, bool) {
	c, ok := inbound[name]
	return c, ok
}

// Outbound 查出站 codec。缺失不是错误：上游可能配了本服务尚未实现出站的协议，
// 调用方应据此把该目标标记为 invalid_model 并换目标。
func Outbound(name string) (OutboundCodec, bool) {
	c, ok := outbound[name]
	return c, ok
}

// InboundNames 与 OutboundNames 供健康检查与管理面报告已装配的协议。
func InboundNames() []string  { return slices.Sorted(maps.Keys(inbound)) }
func OutboundNames() []string { return slices.Sorted(maps.Keys(outbound)) }
