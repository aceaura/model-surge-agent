package httpapi

import (
	"strings"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
)

// 清单接口的返回外形按协议分两族，字段名与包装都不同：
// Anthropic 是 {data:[{id,display_name,type,created_at}],has_more,first_id,last_id}，
// OpenAI 是 {object:"list",data:[{id,object,owned_by,created}]}。
// SDK 解析不出自己认识的形状会直接报错，所以不能只给一种。

type anthropicModel struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
}

type anthropicModelList struct {
	Data    []anthropicModel `json:"data"`
	HasMore bool             `json:"has_more"`
	FirstID string           `json:"first_id,omitempty"`
	LastID  string           `json:"last_id,omitempty"`
}

type openaiModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
	Created int64  `json:"created"`
}

type openaiModelList struct {
	Object string        `json:"object"`
	Data   []openaiModel `json:"data"`
}

type geminiModel struct {
	// Name 带 models/ 前缀：Gemini 的资源名含集合段，客户端会把它原样
	// 回传到请求路径里。
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	// SupportedGenerationMethods 恒为这两项：本服务对每个模型都既支持
	// 一次性生成也支持流式，没有逐模型的差异。
	SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
}

type geminiModelList struct {
	Models []geminiModel `json:"models"`
}

// geminiNamePrefix 是 Gemini 资源名的集合段。
const geminiNamePrefix = "models/"

// modelCreatedAt 是清单里 created 字段的固定取值。
//
// 用固定时间戳而非当前时间：这个字段对本服务没有意义（用户模型是配置项，
// 没有创建时间），但客户端会缓存清单并比较字段，每次请求都变会让它们
// 误以为模型换过。取值是 2024-01-01T00:00:00Z。
const (
	modelCreatedAt   = 1704067200
	modelCreatedText = "2024-01-01T00:00:00Z"
)

// 三个单元素构造函数。清单与单模型查询共用它们，否则两条路径的字段
// 会各自漂移——客户端拿清单里的 id 去单查，两处形状不一致就白费。

func anthropicElem(m relayclient.UserModelSummary) anthropicModel {
	return anthropicModel{
		ID: m.Name, Type: "model", DisplayName: m.Name, CreatedAt: modelCreatedText,
	}
}

func openaiElem(m relayclient.UserModelSummary) openaiModel {
	// owned_by 填 collection：这是本服务里最接近「归属」的概念，
	// 而客户端有时用它给模型分组展示。
	return openaiModel{
		ID: m.Name, Object: "model", OwnedBy: m.Collection, Created: modelCreatedAt,
	}
}

func geminiElem(m relayclient.UserModelSummary) geminiModel {
	return geminiModel{
		Name:        geminiNamePrefix + m.Name,
		DisplayName: m.Name,
		SupportedGenerationMethods: []string{
			"generateContent", "streamGenerateContent",
		},
	}
}

// renderModels 把调度层的清单渲染成指定协议族的外形。
//
// 禁用的模型不列出：客户端会把清单当成可选项展示，列出来只会让用户选到一个必然失败的模型。
func renderModels(family string, models []relayclient.UserModelSummary) any {
	switch family {
	case codec.ProtocolAnthropic:
		out := anthropicModelList{Data: make([]anthropicModel, 0, len(models))}
		for _, m := range models {
			if !m.Enabled {
				continue
			}
			out.Data = append(out.Data, anthropicElem(m))
		}
		if len(out.Data) > 0 {
			out.FirstID = out.Data[0].ID
			out.LastID = out.Data[len(out.Data)-1].ID
		}
		return out

	case codec.ProtocolGemini:
		out := geminiModelList{Models: make([]geminiModel, 0, len(models))}
		for _, m := range models {
			if !m.Enabled {
				continue
			}
			out.Models = append(out.Models, geminiElem(m))
		}
		return out
	}

	out := openaiModelList{Object: "list", Data: make([]openaiModel, 0, len(models))}
	for _, m := range models {
		if !m.Enabled {
			continue
		}
		out.Data = append(out.Data, openaiElem(m))
	}
	return out
}

// renderModel 渲染单个模型对象，外形与清单元素一致。
func renderModel(family string, m relayclient.UserModelSummary) any {
	switch family {
	case codec.ProtocolAnthropic:
		return anthropicElem(m)
	case codec.ProtocolGemini:
		return geminiElem(m)
	default:
		return openaiElem(m)
	}
}

// findModel 按 id 找一个启用的模型。
//
// 禁用的按不存在处理：清单不列它，单查却回它会让客户端拿到一个必然失败的
// id——而它从清单里根本看不到这个 id，无从判断为什么失败。
func findModel(models []relayclient.UserModelSummary, id string) (relayclient.UserModelSummary, bool) {
	for _, m := range models {
		if m.Name == id && m.Enabled {
			return m, true
		}
	}
	return relayclient.UserModelSummary{}, false
}

// modelIDFromPath 取出单模型查询的 id。
//
// 剥 models/ 前缀：Gemini 客户端回传的是清单里的资源名（models/xxx），
// 而其余两族回传裸 id，两种都要落到同一个查找键上。
func modelIDFromPath(raw string) string {
	return strings.TrimPrefix(strings.Trim(raw, "/"), geminiNamePrefix)
}
