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
	"encoding/json"
	"fmt"
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

	out, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("paramover: encode merged body: %w", err)
	}
	return out, nil
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

func decodeObject(raw json.RawMessage, what string) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("paramover: %s must be a json object: %w", what, err)
	}
	return obj, nil
}
