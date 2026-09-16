package httpapi

import (
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

// modelCreatedAt 是清单里 created 字段的固定取值。
//
// 用固定时间戳而非当前时间：这个字段对本服务没有意义（用户模型是配置项，
// 没有创建时间），但客户端会缓存清单并比较字段，每次请求都变会让它们
// 误以为模型换过。取值是 2024-01-01T00:00:00Z。
const (
	modelCreatedAt   = 1704067200
	modelCreatedText = "2024-01-01T00:00:00Z"
)

// renderModels 把调度层的清单渲染成指定协议的外形。
//
// 禁用的模型不列出：客户端会把清单当成可选项展示，列出来只会让用户选到一个必然失败的模型。
func renderModels(protocol string, models []relayclient.UserModelSummary) any {
	if protocol == codec.ProtocolAnthropic {
		out := anthropicModelList{Data: make([]anthropicModel, 0, len(models))}
		for _, m := range models {
			if !m.Enabled {
				continue
			}
			out.Data = append(out.Data, anthropicModel{
				ID: m.Name, Type: "model", DisplayName: m.Name, CreatedAt: modelCreatedText,
			})
		}
		if len(out.Data) > 0 {
			out.FirstID = out.Data[0].ID
			out.LastID = out.Data[len(out.Data)-1].ID
		}
		return out
	}

	out := openaiModelList{Object: "list", Data: make([]openaiModel, 0, len(models))}
	for _, m := range models {
		if !m.Enabled {
			continue
		}
		// owned_by 填 collection：这是本服务里最接近「归属」的概念，
		// 而客户端有时用它给模型分组展示。
		out.Data = append(out.Data, openaiModel{
			ID: m.Name, Object: "model", OwnedBy: m.Collection, Created: modelCreatedAt,
		})
	}
	return out
}
