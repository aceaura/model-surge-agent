package codec

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 轮次49：DescribeResponseClientMetaLoss 的门控——承载族（chat_completions /
// responses）编码时原样带回审核回执与客户端关联键值回声，无损耗、不报；非承载族
//（anthropic 是唯一面向客户端的非承载族，gemini 仅出站）没有对应槽位，真非空时
// 照实报出。字段为空 / moderation 显式 null 时一律静默，绝不误报。

func metaLossResp() *ir.Response {
	return &ir.Response{
		ID: "msg_1", Model: "m",
		ResponsesModeration: json.RawMessage(`{"input":{"flagged":true}}`),
		ClientMetadata:      map[string]string{"trace": "abc"},
	}
}

func TestR49CarriersSilent(t *testing.T) {
	for _, name := range []string{ProtocolChatCompletions, ProtocolResponses} {
		if notes := DescribeResponseClientMetaLoss(metaLossResp(), name); len(notes) != 0 {
			t.Errorf("承载族 %s 被误报 metadata/moderation 丢弃：%v", name, notes)
		}
	}
}

func TestR49NonCarriersNoted(t *testing.T) {
	for _, name := range []string{ProtocolAnthropic, ProtocolGemini} {
		notes := DescribeResponseClientMetaLoss(metaLossResp(), name)
		var mod, meta bool
		for _, n := range notes {
			if strings.Contains(n, "moderation receipt") {
				mod = true
			}
			if strings.Contains(n, "echoed client metadata") {
				meta = true
			}
		}
		if !mod || !meta {
			t.Errorf("非承载族 %s 未报全（mod=%v meta=%v）：%v", name, mod, meta, notes)
		}
	}
}

func TestR49NilAndEmptySilent(t *testing.T) {
	if notes := DescribeResponseClientMetaLoss(nil, ProtocolAnthropic); len(notes) != 0 {
		t.Errorf("nil 响应被误报：%v", notes)
	}
	empty := &ir.Response{ID: "msg_1", Model: "m"}
	if notes := DescribeResponseClientMetaLoss(empty, ProtocolAnthropic); len(notes) != 0 {
		t.Errorf("空字段被误报：%v", notes)
	}
	nullMod := &ir.Response{ID: "msg_1", Model: "m", ResponsesModeration: json.RawMessage(`null`)}
	if notes := DescribeResponseClientMetaLoss(nullMod, ProtocolAnthropic); len(notes) != 0 {
		t.Errorf("显式 null moderation 被误报：%v", notes)
	}
}
