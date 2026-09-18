package ratelimit

import (
	"net/http"
	"testing"
	"time"
)

// base 是所有用例共用的「当下」。固定时刻而非 time.Now()：
// 相对秒数与 epoch 的换算都以它为原点，用真实时钟会让断言变成近似比较。
var base = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

func header(pairs ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(pairs); i += 2 {
		h.Set(pairs[i], pairs[i+1])
	}
	return h
}

// TestResetAtPerHeader 逐头逐形态：每个被认的头单独给一次，
// 确认它真的被读到，而不是靠别的头兜出结果。
func TestResetAtPerHeader(t *testing.T) {
	cases := []struct {
		name string
		h    http.Header
		want time.Time
	}{
		{
			name: "Retry-After 秒数",
			h:    header("Retry-After", "30"),
			want: base.Add(30 * time.Second),
		},
		{
			// 实测有上游回小数秒，ParseInt 会整条解析失败。
			name: "Retry-After 小数秒",
			h:    header("Retry-After", "1.5"),
			want: base.Add(1500 * time.Millisecond),
		},
		{
			name: "Retry-After HTTP-date",
			h:    header("Retry-After", base.Add(2*time.Minute).Format(http.TimeFormat)),
			want: base.Add(2 * time.Minute).Truncate(time.Second),
		},
		{
			name: "anthropic 聚合窗口（秒级 epoch）",
			h: header("anthropic-ratelimit-unified-reset",
				itoa(base.Add(90*time.Minute).Unix())),
			want: base.Add(90 * time.Minute),
		},
		{
			name: "anthropic 5h 窗口",
			h: header("anthropic-ratelimit-unified-5h-reset",
				itoa(base.Add(3*time.Hour).Unix())),
			want: base.Add(3 * time.Hour),
		},
		{
			// 7d 窗口的真实到期远超 24h 上限，这里给一个 24h 内的值：
			// 头本身要被认出来，超限那件事由闸门用例单独测。
			name: "anthropic 7d 窗口",
			h: header("anthropic-ratelimit-unified-7d-reset",
				itoa(base.Add(20*time.Hour).Unix())),
			want: base.Add(20 * time.Hour),
		},
		{
			name: "x-ratelimit-reset-requests（毫秒 epoch）",
			h: header("x-ratelimit-reset-requests",
				itoa(base.Add(10*time.Minute).UnixMilli())),
			want: base.Add(10 * time.Minute),
		},
		{
			name: "x-ratelimit-reset-tokens（相对秒数）",
			h:    header("x-ratelimit-reset-tokens", "45"),
			want: base.Add(45 * time.Second),
		},
		{
			name: "codex primary（相对秒数）",
			h:    header("x-codex-primary-reset-after-seconds", "600"),
			want: base.Add(10 * time.Minute),
		},
		{
			name: "codex secondary（相对秒数）",
			h:    header("x-codex-secondary-reset-after-seconds", "120"),
			want: base.Add(2 * time.Minute),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResetAt(tc.h, base)
			if !got.Equal(tc.want) {
				t.Fatalf("ResetAt = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEpochMagnitudeDisambiguation 三种量级各一格。
//
// 同一个头名在不同上游上出现过不同量级，按头名钉死会解错，
// 所以消歧必须由数值大小决定。
func TestEpochMagnitudeDisambiguation(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want time.Time
	}{
		{"毫秒 epoch", itoa(base.Add(time.Hour).UnixMilli()), base.Add(time.Hour)},
		{"秒 epoch", itoa(base.Add(time.Hour).Unix()), base.Add(time.Hour)},
		{"相对秒数", "3600", base.Add(time.Hour)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResetAt(header("x-ratelimit-reset-requests", tc.raw), base)
			if !got.Equal(tc.want) {
				t.Fatalf("ResetAt = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestResetAtRejectsUntrustworthyValues 闸门：坏数据必须当成「没说」。
//
// 每一格都必须回零值而不是一个「差不多」的时刻：编造会把可用目标锁住。
func TestResetAtRejectsUntrustworthyValues(t *testing.T) {
	cases := []struct {
		name string
		h    http.Header
	}{
		{"空头", http.Header{}},
		{"空值", header("Retry-After", "")},
		{"非数字非日期", header("Retry-After", "soon")},
		{"零秒", header("Retry-After", "0")},
		{"负秒", header("Retry-After", "-5")},
		{"过去的 epoch", header("anthropic-ratelimit-unified-reset",
			itoa(base.Add(-time.Hour).Unix()))},
		{"恰好当下（不晚于 now 即无效）", header("anthropic-ratelimit-unified-reset",
			itoa(base.Unix()))},
		{"超出 24h 上限", header("Retry-After", "90000")},
		{"7d 窗口的真实到期（远超上限）", header("anthropic-ratelimit-unified-7d-reset",
			itoa(base.Add(7*24*time.Hour).Unix()))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResetAt(tc.h, base); !got.IsZero() {
				t.Fatalf("ResetAt = %v，坏数据必须当成没说", got)
			}
		})
	}
}

// TestResetAtTakesEarliest 多头命中取最早。
//
// 取最早只是多一次探测；取最晚会在那个长窗口其实没被拒时白锁数小时。
func TestResetAtTakesEarliest(t *testing.T) {
	h := header(
		"anthropic-ratelimit-unified-7d-reset", itoa(base.Add(20*time.Hour).Unix()),
		"anthropic-ratelimit-unified-5h-reset", itoa(base.Add(3*time.Hour).Unix()),
		"Retry-After", "600",
	)
	want := base.Add(10 * time.Minute)
	if got := ResetAt(h, base); !got.Equal(want) {
		t.Fatalf("ResetAt = %v, want %v（最早的那个）", got, want)
	}
}

// TestResetAtIgnoresUntrustworthyAmongGoodOnes 一个坏值不能连累好值。
//
// 与「取最早」是两件事：坏值若参与比较会被当成最早（零值）或把结果污染成
// 一个假时刻。真实上游常同时回多个窗口头而只有一个有意义。
func TestResetAtIgnoresUntrustworthyAmongGoodOnes(t *testing.T) {
	h := header(
		"Retry-After", "garbage",
		"anthropic-ratelimit-unified-reset", itoa(base.Add(-time.Hour).Unix()),
		"x-codex-primary-reset-after-seconds", "300",
	)
	want := base.Add(5 * time.Minute)
	if got := ResetAt(h, base); !got.Equal(want) {
		t.Fatalf("ResetAt = %v, want %v", got, want)
	}
}

// TestTrustworthyIsExportedForReceivers 接收侧必须能复用同一份判据。
//
// 跨进程传递后要再判一次，两侧判据分成两份代码会漂移。
func TestTrustworthyIsExportedForReceivers(t *testing.T) {
	cases := []struct {
		name string
		t    time.Time
		want bool
	}{
		{"零值", time.Time{}, false},
		{"过去", base.Add(-time.Second), false},
		{"当下", base, false},
		{"一秒后", base.Add(time.Second), true},
		{"恰好 24h", base.Add(24 * time.Hour), true},
		{"超过 24h", base.Add(24*time.Hour + time.Second), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Trustworthy(tc.t, base); got != tc.want {
				t.Fatalf("Trustworthy = %v, want %v", got, tc.want)
			}
		})
	}
}

func itoa(v int64) string {
	const digits = "0123456789"
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf []byte
	for v > 0 {
		buf = append([]byte{digits[v%10]}, buf...)
		v /= 10
	}
	if neg {
		return "-" + string(buf)
	}
	return string(buf)
}
