package ir

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// maxToolNameLen 是四家协议对工具名长度要求的交集。
const maxToolNameLen = 64

// nameHashLen 是超长名截断后附加的短哈希长度。
//
// 带哈希是因为纯截断会把 very_long_..._variant_a 与 very_long_..._variant_b
// 撞成同名，撞名后模型调用哪个都是错的，而这种错没有任何症状可查。
const nameHashLen = 4

// governToolDecls 治理工具声明：空名与重名丢弃，非法字符替换，超长截断。
//
// 与出站协议无关，所以放 IR 层：工具名非法对四家协议都是错，改一次全协议受益。
//
// 改写名字时同步改写消息历史里 tool_use 块的 Name，否则模型看到的历史会
// 引用一个已不存在的工具，下一轮它还会照着旧名字调。
func governToolDecls(r *Request) []string {
	if len(r.Tools) == 0 {
		return nil
	}
	var notes []string
	// renames 记录名字改写，用于同步消息历史。
	renames := map[string]string{}
	seen := map[string]bool{}
	kept := make([]Tool, 0, len(r.Tools))

	for _, t := range r.Tools {
		orig := t.Name
		if strings.TrimSpace(orig) == "" {
			notes = append(notes, "dropped a tool declaration with an empty name")
			continue
		}
		name := sanitizeToolName(orig)
		if name != orig {
			notes = append(notes, fmt.Sprintf("rewrote tool name %q to %q (illegal characters or over %d bytes)", orig, name, maxToolNameLen))
			renames[orig] = name
		}
		if seen[name] {
			notes = append(notes, fmt.Sprintf("dropped a duplicate tool declaration named %q, kept the first", name))
			continue
		}
		seen[name] = true
		t.Name = name
		kept = append(kept, t)
	}

	if len(notes) == 0 {
		// 无畸形就不碰字段：重建切片本身无害，但保持这条纪律能让
		// 「未触发即字节不变」在整条链路上都成立。
		return nil
	}
	r.Tools = kept
	applyToolRenames(r, renames)
	return notes
}

// sanitizeToolName 把名字压进合法字符集与长度上限。
func sanitizeToolName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		if isLegalToolNameRune(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) <= maxToolNameLen {
		return out
	}
	sum := sha256.Sum256([]byte(name))
	suffix := hex.EncodeToString(sum[:])[:nameHashLen]
	return out[:maxToolNameLen-nameHashLen-1] + "_" + suffix
}

// isLegalToolNameRune 给出四家协议接受的字符集交集。
func isLegalToolNameRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '_', r == '-', r == '.':
		return true
	default:
		return false
	}
}

// applyToolRenames 把改写同步到 tool_choice 与消息历史里的 tool_use 块。
//
// 两处同在一个函数里，不拆开：漏掉任一处的症状都是「模型看到的名字与它
// 被要求调的名字不一致」，而两处的修法完全一样，分开写只会让下一个人补了
// 一处忘了另一处。
func applyToolRenames(r *Request, renames map[string]string) {
	// tool_choice 不跟着改的后果最隐蔽：出站整形随后会发现它指向一个
	// 「未声明的工具」而降级成 auto，于是客户端的「必须调这个工具」
	// 变成「模型自己决定」——上游正常回一段文本，没有任何报错。
	if r.ToolChoice != nil && r.ToolChoice.Mode == ToolChoiceTool {
		if to, ok := renames[r.ToolChoice.Name]; ok {
			r.ToolChoice.Name = to
		}
	}
	for mi := range r.Messages {
		for bi := range r.Messages[mi].Content {
			b := &r.Messages[mi].Content[bi]
			if b.Type != BlockToolUse || b.ToolUse == nil {
				continue
			}
			if to, ok := renames[b.ToolUse.Name]; ok {
				b.ToolUse.Name = to
			}
		}
	}
}
