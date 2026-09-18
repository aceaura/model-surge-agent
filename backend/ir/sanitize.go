package ir

import (
	"fmt"
	"strings"
)

// Sanitize 原地修复请求中会被上游拒收的畸形，返回人类可读的诊断说明。
//
// 修的是五类真实会打挂一轮对话的问题：畸形的工具声明（空名、重名、
// 非法字符、超长）、孤儿工具结果、错序的工具结果、无人应答的工具调用、
// 空内容消息。上游对这些普遍回不可重试的 400，
// 而不可重试意味着换目标也救不回来，只能在发出前修好。
//
// 无畸形时不改动任何字段并返回 nil：改动会破坏上游的 prompt cache 前缀，
// 而缓存命中对长会话的成本影响远大于这里省下的几次判断。
func Sanitize(r *Request) []string {
	if r == nil || len(r.Messages) == 0 {
		return nil
	}
	var notes []string
	// 声明治理排在配对治理之前：它会改写工具名并同步历史里的 tool_use，
	// 后续的配对判定应该看到改写后的名字。
	notes = append(notes, governToolDecls(r)...)
	notes = append(notes, pairTools(r)...)
	notes = append(notes, pruneEmpty(r)...)
	return notes
}

// useSite 记录一个 tool_use 块的位置。
type useSite struct {
	msg   int
	block int
}

// pairTools 治理工具调用与结果的配对关系。
func pairTools(r *Request) []string {
	uses, results := indexTools(r)
	if len(uses) == 0 && len(results) == 0 {
		return nil
	}

	var notes []string
	notes = append(notes, demoteOrphans(r, uses)...)
	notes = append(notes, dropUnanswered(r, results)...)
	notes = append(notes, reorderResults(r)...)
	return notes
}

// indexTools 建立全局的调用与结果索引。
//
// 用全局索引而非只比对相邻消息：客户端会在助手消息与工具回复之间插入
// 自己的通知消息，相邻判定会把这类正常配对误判成孤儿，白白破坏一次缓存。
func indexTools(r *Request) (uses map[string]useSite, results map[string]int) {
	uses = map[string]useSite{}
	results = map[string]int{}
	for mi, m := range r.Messages {
		for bi, b := range m.Content {
			switch b.Type {
			case BlockToolUse:
				if b.ToolUse != nil && b.ToolUse.ID != "" {
					uses[b.ToolUse.ID] = useSite{msg: mi, block: bi}
				}
			case BlockToolResult:
				if b.ToolResult != nil {
					results[b.ToolResult.ToolUseID]++
				}
			}
		}
	}
	return uses, results
}

// demoteOrphans 把找不到宣告方的工具结果降级为文本，并去掉重复的结果。
//
// 降级而非丢弃：上下文压缩删掉助手的 tool_use 却留下结果时，结果里的内容
// 仍是模型接下来要用的事实，丢掉等于静默改写上下文。
// 同一个 id 有多份结果时只留最后一份，前面的是重连留下的重放。
func demoteOrphans(r *Request, uses map[string]useSite) []string {
	var notes []string
	// 倒序遍历以便「只留最后一份」：先遇到的即最后一份。
	kept := map[string]bool{}
	for mi := len(r.Messages) - 1; mi >= 0; mi-- {
		content := r.Messages[mi].Content
		for bi := len(content) - 1; bi >= 0; bi-- {
			b := content[bi]
			if b.Type != BlockToolResult || b.ToolResult == nil {
				continue
			}
			id := b.ToolResult.ToolUseID
			if _, ok := uses[id]; !ok {
				content[bi] = demoteToText(b)
				notes = append(notes, fmt.Sprintf("orphan tool_result %q demoted to text", id))
				continue
			}
			if kept[id] {
				content[bi] = demoteToText(b)
				notes = append(notes, fmt.Sprintf("duplicate tool_result %q demoted to text", id))
				continue
			}
			kept[id] = true
		}
	}
	return notes
}

// demoteToText 把工具结果块改写成文本块，保留内容并标注它原本属于哪次调用。
func demoteToText(b Block) Block {
	var text string
	if b.ToolResult != nil {
		text = fmt.Sprintf("[tool result for %s]: %s",
			b.ToolResult.ToolUseID, joinBlockText(b.ToolResult.Content))
	}
	return Block{Type: BlockText, Text: text, CacheCtl: b.CacheCtl}
}

func joinBlockText(blocks []Block) string {
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == BlockText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// dropUnanswered 丢弃没有任何结果应答的工具调用。
//
// 上下文压缩会截掉助手调用之后的整段对话，留下悬空的调用。
// 上游看到「宣告了却没有结果」会拒收整个请求。
func dropUnanswered(r *Request, results map[string]int) []string {
	var notes []string
	var kept []Message
	for _, m := range r.Messages {
		content := make([]Block, 0, len(m.Content))
		for _, b := range m.Content {
			if b.Type == BlockToolUse && b.ToolUse != nil && results[b.ToolUse.ID] == 0 {
				notes = append(notes, fmt.Sprintf("dropped unanswered tool_use %q", b.ToolUse.ID))
				continue
			}
			content = append(content, b)
		}
		// 助手消息可能整条只有那一个调用，删掉后就空了；空消息上游同样拒收。
		if len(content) == 0 && len(m.Content) > 0 {
			notes = append(notes, fmt.Sprintf("dropped %s message left empty by tool_use removal", m.Role))
			continue
		}
		m.Content = content
		kept = append(kept, m)
	}
	if len(notes) > 0 {
		r.Messages = kept
	}
	return notes
}

// reorderResults 把工具结果挪到宣告它们的助手消息之后。
//
// 一条助手消息的多个并行调用，其结果要作为一组连续排列且顺序与调用一致：
// 上游按这个顺序把结果绑回调用，错序会让参数与结果对错。
//
// 做法是无条件摘出所有结果、再按规范布局重插，最后与原布局比对决定是否
// 报告改动。这样不必枚举「哪些情形算已经正确」——本来就正确的输入
// 重插后与原样逐块相同，请求不变、缓存前缀也不动。
func reorderResults(r *Request) []string {
	uses, _ := indexTools(r)
	if len(uses) == 0 {
		return nil
	}

	before := snapshotLayout(r)

	// 按宣告方所在消息分组，组内按调用在消息中的出现顺序排列。
	orderByMsg := map[int][]string{}
	for id, site := range uses {
		orderByMsg[site.msg] = append(orderByMsg[site.msg], id)
	}
	for mi := range orderByMsg {
		sortByBlockIndex(orderByMsg[mi], uses)
	}

	pulled := map[string]Block{}
	for mi := range r.Messages {
		content := r.Messages[mi].Content
		keep := make([]Block, 0, len(content))
		for _, b := range content {
			// 孤儿在前一趟已降级为文本，这里遇到的都有宣告方。
			if b.Type == BlockToolResult && b.ToolResult != nil {
				if _, ok := uses[b.ToolResult.ToolUseID]; ok {
					pulled[b.ToolResult.ToolUseID] = b
					continue
				}
			}
			keep = append(keep, b)
		}
		r.Messages[mi].Content = keep
	}
	if len(pulled) == 0 {
		return nil
	}
	insertGroups(r, orderByMsg, pulled)

	after := snapshotLayout(r)
	if before == after {
		return nil
	}
	notes := make([]string, 0, len(pulled))
	for id := range pulled {
		notes = append(notes, fmt.Sprintf("reordered tool_result %q next to its tool_use", id))
	}
	sortStrings(notes)
	return notes
}

// snapshotLayout 把消息骨架编成一个可比较的字符串：角色、块类型、
// 以及工具块的 id。用它判断重插是否真的改变了布局。
func snapshotLayout(r *Request) string {
	var sb strings.Builder
	for _, m := range r.Messages {
		sb.WriteString(string(m.Role))
		sb.WriteByte('{')
		for _, b := range m.Content {
			sb.WriteString(string(b.Type))
			switch {
			case b.ToolUse != nil:
				sb.WriteByte(':')
				sb.WriteString(b.ToolUse.ID)
			case b.ToolResult != nil:
				sb.WriteByte(':')
				sb.WriteString(b.ToolResult.ToolUseID)
			}
			sb.WriteByte(',')
		}
		sb.WriteString("}|")
	}
	return sb.String()
}

// insertGroups 把每组结果按调用顺序插入宣告方之后的消息。
//
// 倒序处理各组：插入会改变后续消息的下标，从后往前做就不必重算。
func insertGroups(r *Request, orderByMsg map[int][]string, pulled map[string]Block) {
	msgs := make([]int, 0, len(orderByMsg))
	for mi := range orderByMsg {
		msgs = append(msgs, mi)
	}
	sortInts(msgs)

	for i := len(msgs) - 1; i >= 0; i-- {
		mi := msgs[i]
		group := make([]Block, 0, len(orderByMsg[mi]))
		for _, id := range orderByMsg[mi] {
			if b, ok := pulled[id]; ok {
				group = append(group, b)
			}
		}
		if len(group) == 0 {
			continue
		}
		next := mi + 1
		if next < len(r.Messages) && r.Messages[next].Role == RoleUser {
			// 并进已有的用户消息，结果排在最前：上游要求结果紧跟调用。
			r.Messages[next].Content = append(group, r.Messages[next].Content...)
			continue
		}
		// 没有可承载的用户消息就新建一条。
		r.Messages = append(r.Messages, Message{})
		copy(r.Messages[next+1:], r.Messages[next:])
		r.Messages[next] = Message{Role: RoleUser, Content: group}
	}
}

// pruneEmpty 清理空内容消息与助手尾部的空白 prefill。
//
// 空消息与纯空白的助手 prefill 都会让 Anthropic 直接 400
// （"text content blocks must contain non-whitespace text"）。
func pruneEmpty(r *Request) []string {
	var notes []string

	// 尾部助手消息的空白处理要先做：trim 后可能整条变空，交给下面的清理。
	if n := len(r.Messages) - 1; n >= 0 && r.Messages[n].Role == RoleAssistant {
		if trimmed, changed := trimTrailingWhitespace(r.Messages[n].Content); changed {
			r.Messages[n].Content = trimmed
			notes = append(notes, "trimmed trailing whitespace from assistant prefill")
		}
	}

	var kept []Message
	dropped := false
	for i, m := range r.Messages {
		if isEmptyContent(m.Content) {
			notes = append(notes, fmt.Sprintf("dropped empty %s message[%d]", m.Role, i))
			dropped = true
			continue
		}
		kept = append(kept, m)
	}
	if dropped {
		r.Messages = kept
	}
	return notes
}

// trimTrailingWhitespace 去掉最后一个文本块的尾随空白；
// 该块因此变空则整块移除。返回是否有改动。
func trimTrailingWhitespace(content []Block) ([]Block, bool) {
	for i := len(content) - 1; i >= 0; i-- {
		if content[i].Type != BlockText {
			// 最后一块不是文本，没有 prefill 空白问题。
			return content, false
		}
		trimmed := strings.TrimRight(content[i].Text, " \t\r\n")
		if trimmed == content[i].Text {
			return content, false
		}
		if trimmed == "" {
			return content[:i], true
		}
		content[i].Text = trimmed
		return content, true
	}
	return content, false
}

// isEmptyContent 判断一条消息是否没有任何可发送的内容。
// 只含空文本块也算空：上游会拒收。
func isEmptyContent(content []Block) bool {
	for _, b := range content {
		switch b.Type {
		case BlockText:
			if strings.TrimSpace(b.Text) != "" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// sortByBlockIndex 按块在消息内的出现顺序排 id。
func sortByBlockIndex(ids []string, uses map[string]useSite) {
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && uses[ids[j]].block < uses[ids[j-1]].block; j-- {
			ids[j], ids[j-1] = ids[j-1], ids[j]
		}
	}
}

func sortInts(v []int) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

func sortStrings(v []string) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}
