package pipeline

import (
	"net/http"
	"strings"
)

// protectedOutboundHeaders 是配置不得覆盖的出站请求头。
//
// 黑名单而非白名单：白名单要枚举「所有上游可能需要的头」，那个集合是开放的，
// 每接一家新上游都得改代码。黑名单只需枚举「写进去一定坏事的」，
// 而那个集合由一个理由封闭界定——它们由标准库按传输的实际情况计算。
//
// Content-Type 与 Accept 刻意不在其中：它们是意图表达而非传输计算，
// 且确有上游要求 `application/json; charset=utf-8` 或别的 Accept 值。
// 运维覆盖它们是正当的配置行为。
var protectedOutboundHeaders = map[string]string{
	// 这一项最隐蔽。另外四个写错会让请求立刻变形或被上游拒收，一次 400
	// 就暴露了；这一个不会：请求正常发出、上游正常回 200，只是标准库从此
	// 不再透明解压（它只在自己加过这个头时才解），压缩字节进切帧器切不出
	// 任何东西，落到「HTTP 200 却一个事件都没解出来」那条可重试路径上——
	// 而所有目标都配了同样的头，三次全败。
	"accept-encoding": "the transport computes it; setting it disables transparent decompression",
	"content-length":  "the transport computes it",
	// Go 用 Request.Body 与 ContentLength 决定分块传输，手写这个头不生效
	// 反而会让请求头自相矛盾。
	"transfer-encoding": "the transport computes it",
	// Host 要改的是 Request.Host 字段，写进 Header 里对标准库无效。
	"host":       "the transport computes it",
	"connection": "the transport computes it",
}

// setOutboundHeader 写一个来自配置的出站请求头，受保护的丢弃并把说明追加到
// notes，返回新的切片。
//
// 只拦配置来源的头（Endpoint 的 extra、调度层下发的 target.Headers、
// 客户端声明），不拦本服务自己写死的那两个——后者是代码，改它要过评审。
//
// 说明走调用方的 notes 而不是 rec.addResponseLossy：后者是累加的，
// 换目标重试时会留下一个没被采用的目标的配置问题，而这一列描述的是最终
// 发出去的那一次。notes 由 open 在本次尝试内合进 rec.Lossy（覆盖语义）。
//
// 说明刻意不拼头的值：这五个头里不会有凭据，但「不拼值」是一条更容易守住的
// 规则，将来集合扩大时不必重新判断每一项是否敏感。
func setOutboundHeader(req *http.Request, key, value string, notes []string) []string {
	if why, bad := protectedOutboundHeaders[strings.ToLower(key)]; bad {
		// 用规范化的头名而不是配置里的原文：原文大小写各异，
		// 同一个头会散成多条说明。
		return append(notes, "dropped outbound header "+
			http.CanonicalHeaderKey(key)+" from configuration ("+why+")")
	}
	req.Header.Set(key, value)
	return notes
}
