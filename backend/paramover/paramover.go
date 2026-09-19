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
)

// Apply 依次作用 defaults 与 overrides，返回新的请求体。
//
// defaults 缺失才填：键在 body 中不存在时才写入，已存在（含客户端显式传值）则保留。
// overrides 强制压盖：无条件写入，压掉 body 中的同名键。
//
// 两者都只在 object 层递归下钻；数组与标量整体替换，因为数组做元素级合并
// 没有可预测语义。这与配置中心对参数层的定义一致。
func Apply(body, defaults, overrides json.RawMessage) (json.RawMessage, error) {
	if len(defaults) == 0 && len(overrides) == 0 {
		return body, nil
	}

	root, err := decodeObject(body, "request body")
	if err != nil {
		return nil, err
	}
	if root == nil {
		// 请求体不是 object 就无处叠加参数。这只可能是出站 codec 的 bug，
		// 让它显式失败而不是静默丢弃配置。
		return nil, fmt.Errorf("paramover: request body must be a json object")
	}

	def, err := decodeObject(defaults, "defaults")
	if err != nil {
		return nil, err
	}
	over, err := decodeObject(overrides, "overrides")
	if err != nil {
		return nil, err
	}

	applyDefaults(root, def)
	applyOverrides(root, over)

	out, err := encodeObject(root)
	if err != nil {
		return nil, err
	}
	return out, nil
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
