package ratelimit

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

// base 与 header 复用 ratelimit_test.go 里的那两个：同一个包里再立一份
// 「当下」会让两组用例的原点在有人改其中一处时悄悄分叉。
func hdr(name, value string) http.Header { return header(name, value) }

// OpenAI 兼容层普遍用 Go duration 形态表达 reset。
//
// 此前只做 ParseFloat，这些值一律解成零值——而零值与「上游没说」不可区分，
// 于是调度层回落到启发式冷却，而上游其实明确说了。
func TestDurationFormIsParsed(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"1s", time.Second},
		{"6m0s", 6 * time.Minute},
		{"1h30m", 90 * time.Minute},
		{"1.5s", 1500 * time.Millisecond},
		// 亚秒抬到 1 秒：「20 毫秒后重试」在限流语境下几乎总是错的。
		{"20ms", time.Second},
		{"1ns", time.Second},
	}
	for _, c := range cases {
		t.Run(c.raw, func(t *testing.T) {
			got := epochOrOffset(c.raw, base)
			if want := base.Add(c.want); !got.Equal(want) {
				t.Errorf("epochOrOffset(%q) = %v, want %v", c.raw, got, want)
			}
		})
	}
}

// 非正的 duration 仍是零值，与 offsetFrom 拒绝非正数同口径。
func TestNonPositiveDurationIsZero(t *testing.T) {
	for _, raw := range []string{"-5s", "0s", "-1ms"} {
		if got := epochOrOffset(raw, base); !got.IsZero() {
			t.Errorf("epochOrOffset(%q) = %v, want 零值", raw, got)
		}
	}
}

// RFC3339 绝对时刻能解出来。Anthropic 标准限流的三个分窗头用的就是这个形态。
func TestRFC3339FormIsParsed(t *testing.T) {
	want := base.Add(90 * time.Second)
	got := epochOrOffset(want.Format(time.RFC3339), base)
	if !got.Equal(want) {
		t.Errorf("RFC3339 解析 = %v, want %v", got, want)
	}
}

// 纯数字必须仍被当成数字。
//
// 这是回归护栏而非顺序护栏：ParseDuration 拒绝无单位数字（实测报
// missing unit），两支吞不掉对方。钉的是「数字支还在、量级判断还对」。
func TestNumericFormWinsOverDuration(t *testing.T) {
	epoch := base.Add(time.Hour).Unix()
	got := epochOrOffset(strconv.FormatInt(epoch, 10), base)
	if want := time.Unix(epoch, 0).UTC(); !got.Equal(want) {
		t.Errorf("纯数字被当成了 duration: got %v, want %v", got, want)
	}
	// 相对秒数那一支同样不能被 duration 分支抢走。
	if got := epochOrOffset("30", base); !got.Equal(base.Add(30 * time.Second)) {
		t.Errorf("相对秒数 30 = %v, want %v", got, base.Add(30*time.Second))
	}
}

// 毫秒 epoch 与秒 epoch 的量级判断保持原状。
func TestNumericMagnitudesUnchanged(t *testing.T) {
	ms := base.Add(time.Hour)
	if got := epochOrOffset(strconv.FormatInt(ms.UnixMilli(), 10), base); !got.Equal(ms) {
		t.Errorf("毫秒 epoch = %v, want %v", got, ms)
	}
}

// 三种形态都认不出来时返回零值，不猜。
func TestUnrecognizedFormIsZero(t *testing.T) {
	for _, raw := range []string{"soon", "", "  ", "next tuesday", "1s2"} {
		if got := epochOrOffset(raw, base); !got.IsZero() {
			t.Errorf("epochOrOffset(%q) = %v, want 零值", raw, got)
		}
	}
}

// Anthropic 标准限流的三个分窗头各自都要能单独命中。
//
// 单独测每一个而不是遍历那个切片：遍历切片的测试在「某一项被从切片里删掉」
// 时会跟着少测一项，于是永远是绿的。
func TestAnthropicStandardResetHeadersAreRegistered(t *testing.T) {
	want := base.Add(2 * time.Minute)
	for _, name := range []string{
		"anthropic-ratelimit-requests-reset",
		"anthropic-ratelimit-input-tokens-reset",
		"anthropic-ratelimit-output-tokens-reset",
	} {
		t.Run(name, func(t *testing.T) {
			got := ResetAt(hdr(name, want.Format(time.RFC3339)), base)
			if !got.Equal(want) {
				t.Errorf("ResetAt(%s) = %v, want %v", name, got, want)
			}
		})
	}
}

// 新增的两种形态同样受 24h 地平线约束。
//
// 闸门在 ResetAt 而不在 epochOrOffset 里，把它挪进解析函数是一个看起来更
// 整齐的重构，而那样做会让多头取最早各自过一遍闸门、失去统一口径。
func TestNewFormsStillObeyTheHorizon(t *testing.T) {
	far := base.Add(30 * 24 * time.Hour)
	cases := map[string]string{
		"duration": "720h",
		"rfc3339":  far.Format(time.RFC3339),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			// 解析本身要成功——否则这条测试测的是「解不出来」而非「被闸门挡掉」。
			if parsed := epochOrOffset(raw, base); parsed.IsZero() {
				t.Fatalf("%q 没被解析出来，这条用例测不到地平线", raw)
			}
			if got := ResetAt(hdr("x-ratelimit-reset-requests", raw), base); !got.IsZero() {
				t.Errorf("30 天后的值通过了地平线闸门: %v", got)
			}
		})
	}
}

// 多个头混用不同形态时仍取最早，既有语义不变。
func TestEarliestAcrossMixedForms(t *testing.T) {
	h := http.Header{}
	h.Set("x-ratelimit-reset-requests", "10m")
	h.Set("anthropic-ratelimit-requests-reset", base.Add(90*time.Second).Format(time.RFC3339))
	h.Set("x-codex-primary-reset-after-seconds", "600")

	got := ResetAt(h, base)
	if want := base.Add(90 * time.Second); !got.Equal(want) {
		t.Errorf("混形态取最早 = %v, want %v", got, want)
	}
}

// ---- HeaderSeconds ----

// 向上取整：截断会把 1.2 秒写成 1，客户端早到 0.2 秒又吃一个 429。
func TestHeaderSecondsRoundsUp(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{time.Second, "1"},
		{1200 * time.Millisecond, "2"},
		{time.Millisecond, "1"},
		{90 * time.Second, "90"},
		{2500 * time.Millisecond, "3"},
	}
	for _, c := range cases {
		t.Run(c.in.String(), func(t *testing.T) {
			got, ok := HeaderSeconds(base.Add(c.in), base)
			if !ok {
				t.Fatalf("HeaderSeconds(+%v) 说不该发", c.in)
			}
			if got != c.want {
				t.Errorf("HeaderSeconds(+%v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// 已过去的时刻与零值都不发这个头：一个 0 或负数会让 SDK 立刻重来。
func TestHeaderSecondsRefusesUselessValues(t *testing.T) {
	cases := map[string]time.Time{
		"零值":   {},
		"已过去":  base.Add(-time.Minute),
		"就是现在": base,
	}
	for name, at := range cases {
		t.Run(name, func(t *testing.T) {
			if got, ok := HeaderSeconds(at, base); ok {
				t.Errorf("HeaderSeconds 回了 %q，这个值会让客户端立刻重来", got)
			}
		})
	}
}
