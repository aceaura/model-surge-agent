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

// epochOrOffset 把一个数字解释成时刻：毫秒 epoch、秒 epoch、或相对秒数。
//
// 不按头名分派而按量级判断：同一头名在不同上游上出现过不同量级。
func epochOrOffset(raw string, now time.Time) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return time.Time{}
	}
	switch {
	case v >= millisEpochFloor:
		return time.UnixMilli(int64(v)).UTC()
	case v >= secondsEpochFloor:
		return time.Unix(int64(v), 0).UTC()
	default:
		return offsetFrom(v, now)
	}
}

// offsetFrom 把相对秒数加到当下。非正数返回零值——「0 秒后重试」
// 与「没说」无从区分，而当成没说是保守的那一侧。
func offsetFrom(secs float64, now time.Time) time.Time {
	if secs <= 0 {
		return time.Time{}
	}
	return now.Add(time.Duration(secs * float64(time.Second)))
}
