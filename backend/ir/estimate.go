package ir

import (
	"math"
	"unicode"
)

// EstimateMode 决定估算偏差的方向。
//
// 一个系数满足不了两个用途：调度筛候选与用量累计宁可低估（低估只会多留
// 一个候选、少记一点配额），而客户端据 count_tokens 裁上下文宁可高估
// （低估会让它裁完照样被上游以超长拒掉）。方向相反，只能分开。
type EstimateMode int

const (
	// ModeDispatch 低估安全：est_tokens 与用量兜底用它。
	ModeDispatch EstimateMode = iota
	// ModePublic 高估安全：count_tokens 的回答用它。
	ModePublic
)

// 每 rune 的 token 权重。
//
// CJK 必须单独加权。参考实现（new-api service/token_estimator.go:36-46）
// 按厂商分表实测出的 CJK 系数是 Gemini 0.68 / OpenAI 0.85 / Claude 1.21
// ——都远高于「每 4 字符 1 token」的 0.25。
//
// 我们不按厂商分表：count_tokens 要在 dispatch 之前回答，那时还不知道会
// 落到哪个上游。取三家的两端代替：公开方向取最大者之上（1.25），
// 调度方向取最小者之下（0.6）。
const (
	cjkWeightPublic   = 1.25
	cjkWeightDispatch = 0.6
	// 非 CJK 字符：4 是英文文本的常见比值，两个方向共用。
	// 英文下两家的实测系数都贴近 1 词 1 token，没有上调空间。
	otherWeight = 1.0 / 4.0
)

// mediaTokens 是每个媒体块计入的下限。
//
// 不计入不是「没有公式所以算 0」——一张图在任何厂商都至少几百 token，
// 算 0 会让带图请求的估算偏低一个量级。258 取自 Gemini 的固定图片计费，
// 是三家公开值里最小的（new-api 对非 OpenAI 模型用 520），
// 取最小值才能保证它是下限而不是虚高。
//
// 不按像素算：那是 OpenAI 的专有公式（new-api service/token_counter.go:22-174），
// 对另三族无效。
const mediaTokens = 258

func cjkWeight(mode EstimateMode) float64 {
	if mode == ModePublic {
		return cjkWeightPublic
	}
	return cjkWeightDispatch
}

// isCJK 的范围照抄参考实现（new-api service/token_estimator.go:150-155）。
// 那个范围经过实测，自己重划只会引入没验证过的边界。
func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		(r >= 0x3040 && r <= 0x30FF) || // 日文
		(r >= 0xAC00 && r <= 0xD7A3) // 韩文
}

// EstimateTokens 按调度方向估算一段文本，保留原签名给既有调用点。
func EstimateTokens(s string) int64 {
	return EstimateTokensMode(s, ModeDispatch)
}

// EstimateTokensMode 估算一段文本的 token 数。空串为 0，非空至少 1。
func EstimateTokensMode(s string, mode EstimateMode) int64 {
	if s == "" {
		return 0
	}
	w := cjkWeight(mode)
	var total float64
	for _, r := range s {
		if isCJK(r) {
			total += w
			continue
		}
		total += otherWeight
	}
	// 向上取整：这是个界，截断会让每一段文本都少算一点，累加起来方向就跑了。
	n := int64(math.Ceil(total))
	if n < 1 {
		return 1
	}
	return n
}

// EstimateRequest 按调度方向估算输入 token 数，供 est_tokens 与用量兜底使用。
func EstimateRequest(r *Request) int64 {
	return EstimateRequestMode(r, ModeDispatch)
}

// EstimateRequestMode 估算请求的输入 token 数。
func EstimateRequestMode(r *Request, mode EstimateMode) int64 {
	if r == nil {
		return 0
	}
	var total int64
	total += estimateBlocks(r.System, mode)
	for _, m := range r.Messages {
		total += estimateBlocks(m.Content, mode)
	}
	for _, t := range r.Tools {
		total += EstimateTokensMode(t.Name, mode) +
			EstimateTokensMode(t.Description, mode) +
			EstimateTokensMode(t.Schema, mode)
	}
	return total
}

// EstimateResponse 按调度方向估算输出 token 数，供上游未返回 usage 时兜底。
func EstimateResponse(r *Response) int64 {
	if r == nil {
		return 0
	}
	return estimateBlocks(r.Content, ModeDispatch)
}

func estimateBlocks(blocks []Block, mode EstimateMode) int64 {
	var total int64
	for _, b := range blocks {
		total += EstimateTokensMode(b.Text, mode)
		if b.ToolUse != nil {
			total += EstimateTokensMode(b.ToolUse.Name, mode) +
				EstimateTokensMode(b.ToolUse.Input, mode)
		}
		if b.ToolResult != nil {
			total += estimateBlocks(b.ToolResult.Content, mode)
		}
		if b.Thinking != nil {
			total += EstimateTokensMode(b.Thinking.Text, mode)
		}
		if b.Media != nil {
			// base64 长度与 token 消耗无关，按块数计下限。
			total += mediaTokens
		}
	}
	return total
}
