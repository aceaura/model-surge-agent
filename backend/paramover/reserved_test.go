package paramover

import (
	"encoding/json"
	"strings"
	"testing"
)

// 本文件守「overrides 不得改写本服务自己算出来的值」。判据 7–14。

func applyOver(t *testing.T, body, overrides string) (map[string]any, []string) {
	t.Helper()
	out, notes, err := Apply(json.RawMessage(body), nil, json.RawMessage(overrides))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return got, notes
}

// 判据 7：model 是 target.NativeModel，overrides 压不掉。
//
// 压掉的后果不在这次请求上：请求打到另一个模型并按那个模型计费，
// 而流水、上报与轨迹三处记的都是调度层派的 model_id，事后对不出账。
func TestOverrideCannotReplaceModel(t *testing.T) {
	got, notes := applyOver(t, `{"model":"kimi-k3-256k","stream":true}`,
		`{"model":"ghost-model"}`)
	if got["model"] != "kimi-k3-256k" {
		t.Errorf("model = %v，want 保持 native model", got["model"])
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "model") {
		t.Errorf("notes = %#v，want 一条点名 model 的说明", notes)
	}
}

// 判据 8：stream 压不掉。对上游一律流式是桥接层与聚合器的前提。
func TestOverrideCannotDisableStreaming(t *testing.T) {
	got, notes := applyOver(t, `{"model":"m","stream":true}`, `{"stream":false}`)
	if got["stream"] != true {
		t.Errorf("stream = %v，want true", got["stream"])
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "stream") {
		t.Errorf("notes = %#v", notes)
	}
}

// 判据 9：stream_options 与 alt 同样保留。
func TestOverrideCannotTouchUsageOrAlt(t *testing.T) {
	got, notes := applyOver(t,
		`{"model":"m","stream":true,"stream_options":{"include_usage":true}}`,
		`{"stream_options":{"include_usage":false},"alt":"json"}`)
	so, ok := got["stream_options"].(map[string]any)
	if !ok || so["include_usage"] != true {
		t.Errorf("stream_options = %v，want include_usage 仍为 true", got["stream_options"])
	}
	if _, exists := got["alt"]; exists {
		t.Errorf("alt 被写进了请求体：%v", got["alt"])
	}
	if len(notes) != 2 {
		t.Errorf("notes = %#v，want 两条", notes)
	}
}

// 判据 10：跳过而不是报错——一个配错的键不该把整个目标变成死路。
func TestReservedOverrideIsNotAnError(t *testing.T) {
	_, _, err := Apply(json.RawMessage(`{"model":"m"}`), nil,
		json.RawMessage(`{"model":"x"}`))
	if err != nil {
		t.Errorf("保留键被当成错误：%v", err)
	}
}

// 判据 11：只挡顶层。嵌套对象里的同名键是另一回事。
//
// Gemini 的 generationConfig 下就可能出现与顶层同名的键，
// 下钻挡会把正当的调参一起拒掉。
func TestReservedKeysOnlyApplyAtTopLevel(t *testing.T) {
	got, notes := applyOver(t,
		`{"model":"m","generationConfig":{"temperature":0.2}}`,
		`{"generationConfig":{"model":"inner","stream":true}}`)
	gc := got["generationConfig"].(map[string]any)
	if gc["model"] != "inner" || gc["stream"] != true {
		t.Errorf("嵌套键被误挡：%v", gc)
	}
	if len(notes) != 0 {
		t.Errorf("notes = %#v，want 空", notes)
	}
}

// 判据 12：其余键的覆盖行为不变。
func TestOtherOverridesStillApply(t *testing.T) {
	got, notes := applyOver(t, `{"model":"m","temperature":0.9,"top_p":0.5}`,
		`{"temperature":0.1,"model":"x"}`)
	if got["temperature"].(float64) != 0.1 {
		t.Errorf("temperature = %v，want 0.1", got["temperature"])
	}
	if got["top_p"].(float64) != 0.5 {
		t.Errorf("top_p 被动了：%v", got["top_p"])
	}
	if got["model"] != "m" {
		t.Errorf("model = %v", got["model"])
	}
	if len(notes) != 1 {
		t.Errorf("notes = %#v", notes)
	}
}

// 判据 13：defaults 不受此约束——它填不掉已存在的键，构不成改写。
//
// 对它也设保留集会把「上游端点要求显式写 alt」这类正当配置一起拒掉。
func TestDefaultsAreNotRestricted(t *testing.T) {
	out, notes, err := Apply(json.RawMessage(`{"stream":true}`),
		json.RawMessage(`{"model":"filled","alt":"sse"}`), nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["model"] != "filled" || got["alt"] != "sse" {
		t.Errorf("defaults 未生效：%v", got)
	}
	if len(notes) != 0 {
		t.Errorf("defaults 不该出说明：%#v", notes)
	}
}

// 判据 14：说明不拼值——与出站请求头黑名单同口径。
//
// 保留键的值目前都不是凭据，但「不拼值」是更容易守住的规则：
// 一旦开始拼，下一个加进保留集的键就得有人重新判断一次它安不安全。
func TestReservedNoteDoesNotIncludeTheValue(t *testing.T) {
	_, notes := applyOver(t, `{"model":"m"}`, `{"model":"sk-looks-like-a-secret"}`)
	if len(notes) != 1 {
		t.Fatalf("notes = %#v", notes)
	}
	if strings.Contains(notes[0], "sk-looks-like-a-secret") {
		t.Errorf("说明里拼了值：%q", notes[0])
	}
}
