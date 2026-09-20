package codec_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 这个文件守的是调参**取值范围**的跨协议收敛，与 shapematrix_test.go 的
// 「能不能承载」是两件事：字段四个协议都能发，但同一个数字在不同协议里
// 一个合法一个是不可重试的 400。客户端按 OpenAI 习惯发 temperature 1.5
// 是合法入站，被调度到 Anthropic 目标才出问题，而它无从预知这一跳的协议。
//
// 范围只在有确证时才填：anthropic 的上限 1.0 有上游 400 原文
// （temperature: range: 0..1）；OpenAI 与 Gemini 的上限没有实测或官方明示
// 的确证，按既定判据留零值，这里用矩阵钉住「留零值的协议必须原样透传」。

// outboundTemperature 从出站请求体里取出 temperature。
// 第二个返回值为假表示这个协议根本没写出该字段。
func outboundTemperature(t *testing.T, out string, body []byte) (float64, bool) {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("%s: 请求体不是合法 JSON: %v\n%s", out, err, body)
	}
	// gemini 把采样参数装在 generationConfig 里，其余三个在顶层。
	if cfg, ok := obj["generationConfig"].(map[string]any); ok {
		obj = cfg
	}
	v, ok := obj["temperature"]
	if !ok {
		return 0, false
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("%s: temperature 不是数字: %#v", out, v)
	}
	return f, true
}

func tempRequest(v float64) *ir.Request {
	req := shapeRequest()
	req.Temperature = &v
	return req
}

// TestTemperatureClampedToProtocolMaximum 是本轮的核心矩阵：每个出站协议
// 在收到超出自己上限的取值时都必须夹到上限，而不是原样发出去换一个 400。
func TestTemperatureClampedToProtocolMaximum(t *testing.T) {
	const asked = 1.5 // OpenAI 习惯里的合法取值，Anthropic 会拒。
	clamped := 0
	for _, out := range outboundNames() {
		caps := capsOf(t, out)
		body, notes := lossyOf(t, out, tempRequest(asked))
		got, present := outboundTemperature(t, out, body)

		if caps.MaxTemperature <= 0 {
			// 没有确证上限的协议必须原样透传：猜一个上限会把本来能过的
			// 请求改坏，这比偶发的 400 更糟——它无声地改变输出的随机性。
			if present && got != asked {
				t.Errorf("%s 未声明上限却改写了 temperature: %g，want %g", out, got, asked)
			}
			if hasNote(notes, "temperature") {
				t.Errorf("%s 未声明上限却报了 temperature 说明：%v", out, notes)
			}
			continue
		}
		clamped++
		if !present {
			t.Errorf("%s 声明了上限却整个丢掉了 temperature：%s", out, body)
			continue
		}
		if got != caps.MaxTemperature {
			t.Errorf("%s temperature = %g，want 夹到上限 %g", out, got, caps.MaxTemperature)
		}
		// 夹紧改变了输出的随机性，必须让调用方看得见。
		if !hasNote(notes, "temperature") {
			t.Errorf("%s 夹紧了 temperature 却没报说明：%v", out, notes)
		}
	}
	// 守住矩阵本身有效：一个协议都不夹说明这层逻辑整段没被测到。
	if clamped == 0 {
		t.Fatalf("没有任何出站协议声明 MaxTemperature，夹紧逻辑未被覆盖")
	}
}

// TestTemperatureAtProtocolMaximumIsNotRewritten 钉住边界取等号时不动手：
// 上游原文 range: 0..1 是闭区间，把 1.0 也改掉是白报一条说明。
func TestTemperatureAtProtocolMaximumIsNotRewritten(t *testing.T) {
	for _, out := range outboundNames() {
		caps := capsOf(t, out)
		if caps.MaxTemperature <= 0 {
			continue
		}
		body, notes := lossyOf(t, out, tempRequest(caps.MaxTemperature))
		got, present := outboundTemperature(t, out, body)
		if !present || got != caps.MaxTemperature {
			t.Errorf("%s temperature = %g present=%v，want 原样 %g",
				out, got, present, caps.MaxTemperature)
		}
		if hasNote(notes, "temperature") {
			t.Errorf("%s 取值正好等于上限却报了说明：%v", out, notes)
		}
	}
}

// TestTemperatureBelowZeroIsClamped 钉住下界。下界与上界同出一处证据
// （range: 0..1 两端都在这句话里），所以只在声明了上限的协议上生效。
func TestTemperatureBelowZeroIsClamped(t *testing.T) {
	for _, out := range outboundNames() {
		caps := capsOf(t, out)
		if caps.MaxTemperature <= 0 {
			continue
		}
		body, notes := lossyOf(t, out, tempRequest(-0.5))
		got, present := outboundTemperature(t, out, body)
		if !present || got != 0 {
			t.Errorf("%s temperature = %g present=%v，want 夹到 0", out, got, present)
		}
		if !hasNote(notes, "temperature") {
			t.Errorf("%s 夹掉负值却没报说明：%v", out, notes)
		}
		if !strings.Contains(strings.Join(notes, "|"), "minimum") {
			t.Errorf("%s 下界说明未点明是下限：%v", out, notes)
		}
	}
}

// TestTemperatureZeroSurvivesClamping 钉住 0 不被当成「没给」。
// 0 是贪心解码这个明确语义，指针字段区分得了缺席与零值，夹紧逻辑不能把它
// 当边界外的值处理，也不该报说明。
func TestTemperatureZeroSurvivesClamping(t *testing.T) {
	for _, out := range outboundNames() {
		caps := capsOf(t, out)
		if caps.MaxTemperature <= 0 {
			continue
		}
		body, notes := lossyOf(t, out, tempRequest(0))
		got, present := outboundTemperature(t, out, body)
		if !present || got != 0 {
			t.Errorf("%s temperature = %g present=%v，want 原样 0", out, got, present)
		}
		if hasNote(notes, "temperature") {
			t.Errorf("%s 对合法的 0 报了说明：%v", out, notes)
		}
	}
}

// TestTemperatureRangeClampRunsAfterSamplingExclusion 钉住阶段顺序。
// anthropic 开推理时会把 temperature 整个剥掉；若范围夹紧排在剥离之前，
// 就会先夹一个马上要被丢弃的值，白报一条说明并让调用方以为参数还在。
func TestTemperatureRangeClampRunsAfterSamplingExclusion(t *testing.T) {
	for _, out := range outboundNames() {
		caps := capsOf(t, out)
		if caps.MaxTemperature <= 0 || !caps.ThinkingExcludesSampling || !caps.Thinking {
			continue
		}
		req := tempRequest(1.5)
		req.Thinking = &ir.ThinkingConfig{Enabled: ir.ThinkingOn(), BudgetTokens: 4096}
		body, notes := lossyOf(t, out, req)
		if _, present := outboundTemperature(t, out, body); present {
			t.Errorf("%s 开推理时仍写出了 temperature：%s", out, body)
		}
		joined := strings.Join(notes, "|")
		if strings.Contains(joined, "clamped") {
			t.Errorf("%s 对一个随后被剥离的参数报了夹紧说明：%v", out, notes)
		}
		if !hasNote(notes, "temperature/top_p") {
			t.Errorf("%s 未报采样参数被丢弃：%v", out, notes)
		}
	}
}

// TestUnboundedProtocolsDeclareNoGuessedRange 钉住「没确证不猜」这条判据。
// 只有 anthropic 有上游 400 原文，其余三个必须留零值；哪天有了实测证据，
// 这个断言会红，提醒同时更新 docs 与本轮记录。
func TestUnboundedProtocolsDeclareNoGuessedRange(t *testing.T) {
	want := map[string]float64{codec.ProtocolAnthropic: 1.0}
	for _, out := range outboundNames() {
		got := capsOf(t, out).MaxTemperature
		if got != want[out] {
			t.Errorf("%s MaxTemperature = %g，want %g（无确证的上限必须留零值）",
				out, got, want[out])
		}
	}
}
