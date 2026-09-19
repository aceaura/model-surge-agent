// Package paramover 把上游模型配置的两层参数作用到已编码的请求体上。
//
// 作用点在出站 codec 编码之后、发出之前，而不是在 IR 上。这样运维配置的键
// 就是出站协议的原生字段名，能直达协议特有的嵌套结构，无需 IR 为每种协议
// 的每个调参字段建模：
//
//	gemini:    {"generationConfig":{"thinkingConfig":{"thinkingBudget":8192}}}
//	responses: {"reasoning":{"effort":"high"}}
//	anthropic: {"thinking":{"type":"enabled","budget_tokens":16384}}
package paramover

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
)

// Apply 依次作用 defaults 与 overrides，返回新的请求体。
//
// defaults 缺失才填：键在 body 中不存在时才写入，已存在（含客户端显式传值）则保留。
// overrides 强制压盖：无条件写入，压掉 body 中的同名键。
//
// 两者都只在 object 层递归下钻；数组与标量整体替换，因为数组做元素级合并
// 没有可预测语义。这与配置中心对参数层的定义一致。
func Apply(body, defaults, overrides json.RawMessage) (json.RawMessage, []string, error) {
	if len(defaults) == 0 && len(overrides) == 0 {
		return body, nil, nil
	}

	root, err := decodeObject(body, "request body")
	if err != nil {
		return nil, nil, err
	}
	if root == nil {
		// 请求体不是 object 就无处叠加参数。这只可能是出站 codec 的 bug，
		// 让它显式失败而不是静默丢弃配置。
		return nil, nil, fmt.Errorf("paramover: request body must be a json object")
	}

	def, err := decodeObject(defaults, "defaults")
	if err != nil {
		return nil, nil, err
	}
	over, err := decodeObject(overrides, "overrides")
	if err != nil {
		return nil, nil, err
	}

	notes := dropReserved(over)
	applyDefaults(root, def)
	applyOverrides(root, over)

	out, err := encodeObject(root)
	if err != nil {
		return nil, nil, err
	}
	return out, notes, nil
}

// reservedKeys 是 overrides 不得压盖的顶层键。
//
// 它们不是客户端的调参，而是本服务自己算出来的值：model 在编码前被换成了
// target.NativeModel，stream 由三个出站编码器写死为 true（对上游一律流式是
// 整个桥接层与聚合器的前提），stream_options 承载记账要用的 usage 请求，
// alt 是 Gemini 的 SSE 开关。被压掉的症状都不在本次请求上——改 model 会让
// 请求打到另一个模型并按那个模型计费，而流水、上报与轨迹三处记的都是调度层
// 派的 model_id，事后对不出账；改 stream 会让上游回整份 JSON，逐字输出消失
// 而诊断里只有一句「上游忽略了流式请求」，把配置错误归因给了上游。
//
// 只挡顶层：这四个键在四个协议里都在顶层，下钻挡会误伤嵌套对象里的同名键。
// 不做成可配置：能配就能关，而关掉它就回到现状。
var reservedKeys = map[string]string{
	"model":          "the target's native model name",
	"stream":         "the upstream streaming mode",
	"stream_options": "the upstream usage request",
	"alt":            "the Gemini SSE switch",
}

// dropReserved 从 overrides 里摘掉保留键，返回说明。
//
// 跳过而不是报错：报错会让一个配错的键把整个目标变成死路，而 overrides 里
// 绝大多数键是正当的。说明点名键与后果——运维唯一的线索就是这条说明，
// 症状本身不在这次请求上。说明里**不拼值**（与出站请求头黑名单同口径）。
func dropReserved(over map[string]any) []string {
	if len(over) == 0 {
		return nil
	}
	var notes []string
	for k, what := range reservedKeys {
		if _, ok := over[k]; !ok {
			continue
		}
		delete(over, k)
		notes = append(notes, "ignored override of "+k+": it is "+what+
			", not a client parameter")
	}
	sort.Strings(notes)
	return notes
}

// encodeObject 把合并后的对象编回请求体字节。
//
// 关掉 HTML 转义：默认开着的话 prompt 里的 `<`、`>`、`&` 会变成
// `\u003c` 这类六字节转义，语义不变但字节变了——而本服务有两处按字节办事的
// 东西（请求体字节预算、转换四体捕获），同一个请求配了 overrides 与没配，
// 量出来的数就不一样。
//
// Encoder 会在末尾追加换行，必须去掉：这个返回值是要当请求体发出去的。
func encodeObject(obj map[string]any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(obj); err != nil {
		return nil, fmt.Errorf("paramover: encode merged body: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// applyDefaults 只填空缺。键已存在且两侧都是 object 时递归下钻，
// 这样 {"generationConfig":{"temperature":0.6}} 不会整块替换掉
// body 里已有的 generationConfig 其他字段。
func applyDefaults(dst, src map[string]any) {
	for k, v := range src {
		existing, ok := dst[k]
		if !ok {
			dst[k] = v
			continue
		}
		dstChild, dstIsObj := existing.(map[string]any)
		srcChild, srcIsObj := v.(map[string]any)
		if dstIsObj && srcIsObj {
			applyDefaults(dstChild, srcChild)
		}
		// 其余情况保留 dst：defaults 不覆盖已有值。
	}
}

// applyOverrides 无条件压盖，object 对 object 时递归以保留 body 的其他兄弟键。
func applyOverrides(dst, src map[string]any) {
	for k, v := range src {
		existing, ok := dst[k]
		if !ok {
			dst[k] = v
			continue
		}
		dstChild, dstIsObj := existing.(map[string]any)
		srcChild, srcIsObj := v.(map[string]any)
		if dstIsObj && srcIsObj {
			applyOverrides(dstChild, srcChild)
			continue
		}
		dst[k] = v
	}
}

// decodeObject 解出一个 JSON 对象，数字保持原始字面。
//
// UseNumber 让数字停在 json.Number（就是原始字面字符串）而不经过 float64。
// 不开的话 seed 这类大整数会被静默改写：13835058055282163712 出去变成
// 13835058055282164000，上游按另一个种子生成，而请求 200、流水正常、
// 有损诊断为空——只有配了 defaults/overrides 的目标会这样。
//
// Decoder 比 json.Unmarshal 宽松：读完第一个文档就返回，`{"a":1} {"b":2}`
// 会被当成 `{"a":1}`。所以后面要显式确认没有尾随文档，否则这次改动会顺手
// 放宽一个原本守住的边界。
func decodeObject(raw json.RawMessage, what string) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil, fmt.Errorf("paramover: %s must be a json object: %w", what, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("paramover: %s must be a single json object", what)
	}
	return obj, nil
}
