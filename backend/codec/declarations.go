package codec

import "strings"

// Declarations 是客户端随请求发来的协议声明。
//
// 与 ir.Request 分开：这些是「用哪一版协议对话」的传输层元数据，
// 只有 anthropic 出站承载得了，其余三个协议连放它的字段都没有。
// 放进 IR 会让四个 codec 都得回答「我丢了它吗」，包括三个不该关心它的。
type Declarations struct {
	// APIVersion 是客户端声明的协议版本，空表示没声明。
	//
	// 刻意不在这里填默认值：默认值是出站侧的事，在入站假造一个客户端
	// 没说过的声明，会让「客户端要的」与「我们替它要的」分不开。
	APIVersion string

	// Betas 是去重后的特性令牌，保留客户端给出的首次出现顺序。
	Betas []string
}

func (d Declarations) Empty() bool { return d.APIVersion == "" && len(d.Betas) == 0 }

// ParseBetas 把列表值头的多行原始值解析成去重保序的令牌。
//
// 收 []string 而不是 string：anthropic-beta 是列表值头，客户端可以分多行
// 发，只取第一行会静默丢掉其余声明。
func ParseBetas(values []string) []string {
	var out []string
	seen := make(map[string]struct{})
	for _, line := range values {
		for _, tok := range strings.Split(line, ",") {
			tok = strings.TrimSpace(tok)
			if tok == "" {
				continue
			}
			if _, dup := seen[tok]; dup {
				continue
			}
			seen[tok] = struct{}{}
			out = append(out, tok)
		}
	}
	return out
}

// DeclarationEncoder 由能承载客户端协议声明的出站 codec 实现。
//
// 做成可选接口而不是在公共路径判协议名：判断会在新增出站协议时被漏掉，
// 漏了就是把 Anthropic 的头发给不认识它的上游；而不实现接口不会。
type DeclarationEncoder interface {
	DeclarationHeaders(d Declarations) map[string]string
}

// DescribeDeclarationLoss 报告出站 codec 承载不了的声明。
//
// 只在客户端确实声明过时报：没声明就没有损失，报了会让每条非 anthropic
// 出站的流水都挂一条噪声。
func DescribeDeclarationLoss(d Declarations, outbound OutboundCodec) []string {
	if d.Empty() {
		return nil
	}
	if _, ok := outbound.(DeclarationEncoder); ok {
		return nil
	}
	name := ""
	if outbound != nil {
		name = outbound.Name()
	}
	var notes []string
	if d.APIVersion != "" {
		notes = append(notes, "anthropic-version: "+name+" carries no protocol version declaration")
	}
	if len(d.Betas) > 0 {
		notes = append(notes, "anthropic-beta: "+name+" carries no feature declarations")
	}
	return notes
}
