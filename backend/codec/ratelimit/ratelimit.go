// Package ratelimit 把上游限流响应头翻译成「最早可以再来」的绝对时刻。
//
// 独立成包而不放 codec 根：这里没有任何协议知识，四个协议共用同一套。
// 限流头是 HTTP 层的东西，与 KindForStatus 按状态码归类同理由。
package ratelimit

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxResetHorizon 是能接受的最远到期时刻。
//
// 上游时钟不一定与本机同步，一个半年后的时刻是坏数据而不是真的限流——
// 采信它会把一个可用目标锁到下个季度。
const maxResetHorizon = 24 * time.Hour

// resetHeaders 是各家表达「什么时候能再来」的头，按来源分组。
//
// 值是时刻还是相对秒数各家不同，靠 epochOrOffset 消歧，而不是按头名分派：
// 同一个头名在不同上游上出现过不同量级（xAI 与 OpenAI 的 x-ratelimit-reset-*
// 就是一个用相对秒数一个用 epoch），按头名钉死会解错。
var resetHeaders = []string{
	// Anthropic 聚合窗口与分窗。
	"anthropic-ratelimit-unified-reset",
	"anthropic-ratelimit-unified-5h-reset",
	"anthropic-ratelimit-unified-7d-reset",
	// Anthropic 标准限流的三个分窗，值是 RFC3339。
	// 它们与 unified 那三个并列而非互斥：同一次响应上可能只回其中一组。
	"anthropic-ratelimit-requests-reset",
	"anthropic-ratelimit-input-tokens-reset",
	"anthropic-ratelimit-output-tokens-reset",
	// OpenAI 兼容 / xAI。
	"x-ratelimit-reset-requests",
	"x-ratelimit-reset-tokens",
	// Codex：相对秒数。
	"x-codex-primary-reset-after-seconds",
	"x-codex-secondary-reset-after-seconds",
}

// ResetAt 解析出上游明示的最早可重试时刻，拿不到可信值时返回零值。
//
// 多个头同时命中时取**最早**的那个。取最早只是多一次探测，而取最晚会在
// 那个长窗口其实没被拒时白锁数天——保守方向是最早。
func ResetAt(h http.Header, now time.Time) time.Time {
	var best time.Time
	consider := func(t time.Time) {
		if !trustworthy(t, now) {
			return
		}
		if best.IsZero() || t.Before(best) {
			best = t
		}
	}

	consider(parseRetryAfter(h.Get("Retry-After"), now))
	for _, name := range resetHeaders {
		consider(epochOrOffset(h.Get(name), now))
	}
	return best
}

// Trustworthy 判断一个到期时刻是否可采信，供跨进程接收方复用。
//
// 导出是必要的：agent 校验过的时刻在到达调度层时已经过了排队与网络往返，
// 接收侧必须再判一次，而两侧的判据必须是同一份代码。
func Trustworthy(t, now time.Time) bool { return trustworthy(t, now) }

func trustworthy(t, now time.Time) bool {
	if t.IsZero() || !t.After(now) {
		return false
	}
	return !t.After(now.Add(maxResetHorizon))
}

// parseRetryAfter 双解 RFC 9110 的 Retry-After：十进制秒或 HTTP-date。
//
// 秒数允许小数：sub2api 用 ParseFloat 而非 ParseInt，实测有上游回 "1.5"。
func parseRetryAfter(raw string, now time.Time) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if secs, err := strconv.ParseFloat(raw, 64); err == nil {
		return offsetFrom(secs, now)
	}
	if t, err := http.ParseTime(raw); err == nil {
		return t
	}
	return time.Time{}
}

// epoch 边界：1e9 ≈ 2001-09-09，1e11 ≈ 5138 年。
// 两端都远离真实的相对秒数（限流窗口不会有 31 年）与真实的秒级 epoch。
const (
	secondsEpochFloor = 1e9
	millisEpochFloor  = 1e11
)

// minOffset 是相对时长形态的下限。
//
// 「20 毫秒后重试」在限流语境下几乎总是错的：上游给出这个值是因为它按窗口
// 边界算，而客户端与本服务之间还有排队。抬到 1 秒的代价是多等不到一秒，
// 不抬的代价是一次注定再被拒的往返。sub2api 的 quota.go 做同样处置。
const minOffset = time.Second

// epochOrOffset 把 reset 头的值解释成时刻，识别三种形态。
//
// 三种形态互不相交，因此判定顺序不影响结果（本机探针实测：
// time.ParseDuration 对无单位数字报 "missing unit" 而不是当成纳秒，
// 唯一的交集是 "0"，而两条路都把它判成零值）。排成这个顺序只是从最便宜、
// 最常见的一种开始。
//
// 数字形态不按头名分派而按量级判断：同一头名在不同上游上出现过不同量级。
//
// 不在这里判可信度：地平线闸门在 ResetAt 里，多个头取最早必须共用同一份
// 判据，把闸门挪进来会让每个头各自过一遍而 ResetAt 失去统一口径。
func epochOrOffset(raw string, now time.Time) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if v, err := strconv.ParseFloat(raw, 64); err == nil {
		switch {
		case v >= millisEpochFloor:
			return time.UnixMilli(int64(v)).UTC()
		case v >= secondsEpochFloor:
			return time.Unix(int64(v), 0).UTC()
		default:
			return offsetFrom(v, now)
		}
	}
	// OpenAI 兼容层普遍回 "1s"、"6m0s"、"20ms" 这种形态。此前解不出来，
	// 而解不出来与「上游没说」不可区分，于是调度层回落到启发式冷却。
	if d, err := time.ParseDuration(raw); err == nil {
		if d <= 0 {
			return time.Time{}
		}
		if d < minOffset {
			d = minOffset
		}
		return now.Add(d)
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

// HeaderSeconds 把一个到期时刻渲染成 Retry-After 头的值，第二个返回值为假
// 表示不该发这个头。
//
// 从时刻现算而不是转发上游的头字符串：上游那个字节串可能含 CRLF 或长得离谱，
// 而本服务手上已经有一个解析过、过了地平线闸门的时刻值。
//
// 向上取整而不是截断：截断会把 1.2 秒写成 1，客户端早到 0.2 秒又吃一个 429，
// 而这个头存在的全部意义就是让它不必再吃那一次。
//
// 时刻已过去时不发。一个 0 或负数会让 SDK 立刻重来，比不发这个头更坏。
// 零值时刻不必单独判：它是公元 1 年，落在同一个「已过去」里。
func HeaderSeconds(t, now time.Time) (string, bool) {
	d := t.Sub(now)
	if d <= 0 {
		return "", false
	}
	secs := int64(d / time.Second)
	if d%time.Second != 0 {
		secs++
	}
	return strconv.FormatInt(secs, 10), true
}

// offsetFrom 把相对秒数加到当下。非正数返回零值——「0 秒后重试」
// 与「没说」无从区分，而当成没说是保守的那一侧。
func offsetFrom(secs float64, now time.Time) time.Time {
	if secs <= 0 {
		return time.Time{}
	}
	return now.Add(time.Duration(secs * float64(time.Second)))
}
