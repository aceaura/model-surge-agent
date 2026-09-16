package ir

import "unicode/utf8"

// 估算用的每 token 字符数。真实分词器因模型而异且需要词表，
// 这里只求量级正确：est_tokens 用于策略脚本按上下文窗口筛候选，
// usage 兜底只在上游没给数字时使用。
//
// 4 是英文文本的常见比值。中文实测约 1.7 字符/token，故按字符数估算
// 中文会偏低——这个方向是安全的：EstimateRequest 宁可低估上下文占用，
// 让策略不至于因为估算虚高而错误地排除本可用的目标。
const charsPerToken = 4

// EstimateTokens 估算一段文本的 token 数。空串为 0，非空至少 1。
func EstimateTokens(s string) int64 {
	if s == "" {
		return 0
	}
	n := int64(utf8.RuneCountInString(s)) / charsPerToken
	if n < 1 {
		return 1
	}
	return n
}

// EstimateRequest 估算请求的输入 token 数，供 dispatch 的 est_tokens 使用。
func EstimateRequest(r *Request) int64 {
	if r == nil {
		return 0
	}
	var total int64
	total += estimateBlocks(r.System)
	for _, m := range r.Messages {
		total += estimateBlocks(m.Content)
	}
	for _, t := range r.Tools {
		total += EstimateTokens(t.Name) + EstimateTokens(t.Description) + EstimateTokens(t.Schema)
	}
	return total
}

// EstimateResponse 估算响应的输出 token 数，供上游未返回 usage 时兜底。
func EstimateResponse(r *Response) int64 {
	if r == nil {
		return 0
	}
	return estimateBlocks(r.Content)
}

func estimateBlocks(blocks []Block) int64 {
	var total int64
	for _, b := range blocks {
		total += EstimateTokens(b.Text)
		if b.ToolUse != nil {
			total += EstimateTokens(b.ToolUse.Name) + EstimateTokens(b.ToolUse.Input)
		}
		if b.ToolResult != nil {
			total += estimateBlocks(b.ToolResult.Content)
		}
		if b.Thinking != nil {
			total += EstimateTokens(b.Thinking.Text)
		}
		// 图片不按字符估算：base64 长度与 token 消耗无关，
		// 各家按分块数计费，缺乏统一公式，故不计入。
	}
	return total
}
