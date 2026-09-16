package paramover

import (
	"encoding/json"
	"testing"
)

// apply 是测试辅助：跑一遍 Apply 并把结果解成 map 方便断言。
func apply(t *testing.T, body, defaults, overrides string) map[string]any {
	t.Helper()
	out, err := Apply(json.RawMessage(body), raw(defaults), raw(overrides))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal result %s: %v", out, err)
	}
	return got
}

func raw(s string) json.RawMessage {
	if s == "" {
		return nil
	}
	return json.RawMessage(s)
}

func TestDefaultsFillOnlyMissingKeys(t *testing.T) {
	got := apply(t,
		`{"model":"k3","temperature":0.1}`,
		`{"temperature":0.6,"top_p":0.9}`,
		"")
	if got["temperature"] != 0.1 {
		t.Errorf("temperature = %v, an explicit value must survive defaults", got["temperature"])
	}
	if got["top_p"] != 0.9 {
		t.Errorf("top_p = %v, defaults should fill the gap", got["top_p"])
	}
}

func TestOverridesWinOverExplicitValues(t *testing.T) {
	got := apply(t,
		`{"model":"k3","max_tokens":100}`,
		"",
		`{"max_tokens":8192}`)
	if got["max_tokens"] != float64(8192) {
		t.Errorf("max_tokens = %v, overrides must win", got["max_tokens"])
	}
}

func TestOverridesWinOverDefaults(t *testing.T) {
	got := apply(t, `{}`, `{"temperature":0.6}`, `{"temperature":1.0}`)
	if got["temperature"] != 1.0 {
		t.Errorf("temperature = %v, overrides apply after defaults", got["temperature"])
	}
}

// 出站协议特有字段藏在嵌套结构里，这是把覆盖放在 wire body 层的全部理由。
func TestNestedObjectsMergeInsteadOfReplacing(t *testing.T) {
	got := apply(t,
		`{"generationConfig":{"temperature":0.2,"topP":0.8}}`,
		`{"generationConfig":{"thinkingConfig":{"thinkingBudget":8192}}}`,
		`{"generationConfig":{"temperature":0.9}}`)

	cfg, ok := got["generationConfig"].(map[string]any)
	if !ok {
		t.Fatalf("generationConfig = %#v", got["generationConfig"])
	}
	if cfg["temperature"] != 0.9 {
		t.Errorf("temperature = %v, override should reach into the nested object", cfg["temperature"])
	}
	if cfg["topP"] != 0.8 {
		t.Errorf("topP = %v, sibling keys must survive a nested merge", cfg["topP"])
	}
	thinking, ok := cfg["thinkingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("thinkingConfig = %#v, defaults should add a whole missing subtree", cfg["thinkingConfig"])
	}
	if thinking["thinkingBudget"] != float64(8192) {
		t.Errorf("thinkingBudget = %v", thinking["thinkingBudget"])
	}
}

func TestArraysReplaceWholesale(t *testing.T) {
	got := apply(t,
		`{"stop":["a","b"]}`,
		"",
		`{"stop":["z"]}`)
	stop, ok := got["stop"].([]any)
	if !ok || len(stop) != 1 || stop[0] != "z" {
		t.Errorf("stop = %#v, arrays replace rather than merge", got["stop"])
	}
}

func TestArrayDefaultDoesNotDescendIntoElements(t *testing.T) {
	got := apply(t,
		`{"stop":["a"]}`,
		`{"stop":["x","y"]}`,
		"")
	stop, ok := got["stop"].([]any)
	if !ok || len(stop) != 1 || stop[0] != "a" {
		t.Errorf("stop = %#v, an existing array must survive defaults intact", got["stop"])
	}
}

// 类型不一致时 override 整体替换，不试图合并 object 与标量。
func TestOverrideReplacesMismatchedTypes(t *testing.T) {
	got := apply(t,
		`{"thinking":"off"}`,
		"",
		`{"thinking":{"type":"enabled","budget_tokens":16384}}`)
	th, ok := got["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("thinking = %#v", got["thinking"])
	}
	if th["budget_tokens"] != float64(16384) {
		t.Errorf("budget_tokens = %v", th["budget_tokens"])
	}
}

// 两层都空时原样返回，不做一趟无谓的 unmarshal/marshal——
// 那会重排键序，让黄金文件测试莫名失败。
func TestEmptyLayersReturnBodyUntouched(t *testing.T) {
	body := json.RawMessage(`{"model":"k3","b":1,"a":2}`)
	for _, tc := range []struct{ defaults, overrides string }{
		{"", ""},
		{"", "{}"},
		{"{}", ""},
	} {
		out, err := Apply(body, raw(tc.defaults), raw(tc.overrides))
		if err != nil {
			t.Fatalf("Apply(%q,%q): %v", tc.defaults, tc.overrides, err)
		}
		if tc.defaults == "" && tc.overrides == "" && string(out) != string(body) {
			t.Errorf("out = %s, want verbatim body", out)
		}
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("unmarshal %s: %v", out, err)
		}
		if got["model"] != "k3" {
			t.Errorf("body content lost: %s", out)
		}
	}
}

func TestRejectsNonObjectInputs(t *testing.T) {
	cases := []struct {
		name                    string
		body, defaults, overrid string
	}{
		{"body is array", `[1,2]`, `{"a":1}`, ""},
		{"body is scalar", `"text"`, `{"a":1}`, ""},
		{"defaults is array", `{}`, `[1]`, ""},
		{"overrides is scalar", `{}`, "", `5`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Apply(json.RawMessage(tc.body), raw(tc.defaults), raw(tc.overrid)); err == nil {
				t.Error("want an error, config problems must not be swallowed")
			}
		})
	}
}

// 配置中心把两层原样下发正是为了这个区别：合并成一层就再也分不出
// 哪个键该让位给客户端、哪个键该压掉客户端。
func TestLayerSemanticsDifferOnTheSameKey(t *testing.T) {
	asDefault := apply(t, `{"temperature":0.1}`, `{"temperature":0.6}`, "")
	asOverride := apply(t, `{"temperature":0.1}`, "", `{"temperature":0.6}`)
	if asDefault["temperature"] != 0.1 || asOverride["temperature"] != 0.6 {
		t.Errorf("default kept %v, override kept %v; the two layers must not behave alike",
			asDefault["temperature"], asOverride["temperature"])
	}
}
