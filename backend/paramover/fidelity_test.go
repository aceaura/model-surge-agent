package paramover

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// 叠加参数不得改写 body 里未被覆盖的数值。
//
// 此前走 map[string]any 往返且未开 UseNumber，所有数字过一遍 float64：
// seed 13835058055282163712 出去变成 13835058055282164000。上游按另一个种子
// 生成，而请求 200、流水正常、有损诊断为空——且只有配了 defaults/overrides
// 的目标会这样，没配的走空短路原样返回，两边行为不一致。
func TestLargeIntegersSurviveVerbatim(t *testing.T) {
	cases := []string{
		`13835058055282163712`,
		`9007199254740993`, // 2^53+1，float64 的第一个表示不出来的整数
		`-9007199254740993`,
		`0.70000000000000001`,
		`1e3`,
		`1.7976931348623157e+308`,
	}
	for _, num := range cases {
		t.Run(num, func(t *testing.T) {
			body := json.RawMessage(`{"model":"m","seed":` + num + `}`)
			out, _, err := Apply(body, json.RawMessage(`{"temperature":0.6}`), nil)
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if !strings.Contains(string(out), `"seed":`+num) {
				t.Errorf("seed 被改写了\n got = %s\nwant 含 \"seed\":%s", out, num)
			}
		})
	}
}

// 转义形式不得变。
//
// 语义上无害（解回来是同一个字符串），但这是字节层的改写，而本服务有两处
// 按字节办事的东西：请求体字节预算与转换四体捕获。同一个请求配了 overrides
// 与没配，量出来的字节数和抓到的上游请求体就不一样。
func TestHTMLCharactersAreNotEscaped(t *testing.T) {
	body := json.RawMessage(`{"model":"m","system":"if a<b && c>d"}`)
	out, _, err := Apply(body, json.RawMessage(`{"temperature":0.6}`), nil)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if strings.Contains(string(out), `\u003c`) || strings.Contains(string(out), `\u0026`) {
		t.Errorf("HTML 转义没关掉: %s", out)
	}
	if !strings.Contains(string(out), `a<b && c>d`) {
		t.Errorf("原文没保住: %s", out)
	}
}

// 返回值要能直接当请求体发出去：Encoder 追加的换行必须去掉。
func TestOutputHasNoTrailingNewline(t *testing.T) {
	out, _, err := Apply(json.RawMessage(`{"model":"m"}`),
		json.RawMessage(`{"temperature":0.6}`), nil)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if strings.HasSuffix(string(out), "\n") {
		t.Errorf("请求体带了尾随换行: %q", out)
	}
}

// 尾随第二个 JSON 文档仍必须报错。
//
// 这条是本轮改动的副作用护栏：Decoder 比 json.Unmarshal 宽松，读完第一个
// 文档就返回，`{"a":1} {"b":2}` 会被静默当成 `{"a":1}`。改用 Decoder 若不
// 显式确认尾随内容，就顺手放宽了一个原本守住的边界。
func TestTrailingDocumentIsRejected(t *testing.T) {
	cases := map[string]json.RawMessage{
		"body":      json.RawMessage(`{"model":"m"} {"model":"n"}`),
		"defaults":  json.RawMessage(`{"a":1} {"b":2}`),
		"overrides": json.RawMessage(`{"a":1} 7`),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			var err error
			switch name {
			case "body":
				_, _, err = Apply(raw, json.RawMessage(`{"t":1}`), nil)
			case "defaults":
				_, _, err = Apply(json.RawMessage(`{"model":"m"}`), raw, nil)
			case "overrides":
				_, _, err = Apply(json.RawMessage(`{"model":"m"}`), nil, raw)
			}
			if err == nil {
				t.Error("尾随文档被静默接受了")
			}
		})
	}
}

// 非 object 的输入仍必须报错，不因换了解码方式而放过。
func TestNonObjectIsStillRejected(t *testing.T) {
	cases := map[string]json.RawMessage{
		"array":  json.RawMessage(`[1,2]`),
		"scalar": json.RawMessage(`7`),
		"null":   json.RawMessage(`null`),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := Apply(raw, json.RawMessage(`{"t":1}`), nil); err == nil {
				t.Errorf("%s 形态的 body 被接受了", name)
			}
		})
	}
}

// 合并语义不变：overrides 压盖、defaults 只填空缺、object 递归下钻。
//
// 本轮只换解码与编码的方式，语义一个字都不该动。
func TestMergeSemanticsUnchanged(t *testing.T) {
	body := json.RawMessage(
		`{"model":"m","temperature":0.2,"generationConfig":{"topK":40}}`)
	out, _, err := Apply(body,
		json.RawMessage(`{"temperature":0.9,"max_tokens":1024}`),
		json.RawMessage(`{"generationConfig":{"topP":0.95}}`))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("产物不是合法 JSON: %v", err)
	}
	// defaults 不压盖已有值。
	if fmt.Sprint(got["temperature"]) != "0.2" {
		t.Errorf("temperature = %v，defaults 压盖了已有值", got["temperature"])
	}
	// defaults 填空缺。
	if fmt.Sprint(got["max_tokens"]) != "1024" {
		t.Errorf("max_tokens = %v，defaults 没填上", got["max_tokens"])
	}
	// overrides 递归下钻，不整块替换。
	gc, ok := got["generationConfig"].(map[string]any)
	if !ok {
		t.Fatalf("generationConfig 形态变了: %T", got["generationConfig"])
	}
	if fmt.Sprint(gc["topK"]) != "40" {
		t.Errorf("topK 丢了：overrides 整块替换掉了 generationConfig")
	}
	if fmt.Sprint(gc["topP"]) != "0.95" {
		t.Errorf("topP = %v，overrides 没写进去", gc["topP"])
	}
}

// 无配置时的空短路仍然是逐字节原样返回。
//
// 这一支是「配了参数」与「没配参数」两条路的基准：它变了的话，本轮追求的
// 「两条路字节一致」就无从判断。
func TestNoConfigReturnsBodyVerbatim(t *testing.T) {
	body := json.RawMessage(`{"seed":13835058055282163712,"s":"a<b"}`)
	out, _, err := Apply(body, nil, nil)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("空配置改动了请求体\n got = %s\nwant = %s", out, body)
	}
}

// 配了参数与没配参数，body 里原有部分的字节形式必须一致。
//
// 这是本轮两条要求（数值保真、转义保真）合起来的判据，也是唯一能把
// 「同一个请求在两个目标上量出不同字节数」这件事钉住的形式。
func TestConfiguredAndUnconfiguredAgreeOnOriginalBytes(t *testing.T) {
	body := json.RawMessage(`{"seed":13835058055282163712,"s":"a<b && c>d"}`)
	withCfg, _, err := Apply(body, json.RawMessage(`{"temperature":0.6}`), nil)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	for _, frag := range []string{`"seed":13835058055282163712`, `"s":"a<b && c>d"`} {
		if !strings.Contains(string(withCfg), frag) {
			t.Errorf("配了参数之后 %s 变了形\n got = %s", frag, withCfg)
		}
	}
}
