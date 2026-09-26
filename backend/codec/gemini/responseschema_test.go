package gemini_test

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/codec/gemini"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// respSchemaRequest 走 gemini 出站编码，返回请求体与有损说明。
func respSchemaRequest(t *testing.T, rf *ir.ResponseFormat) (string, []string) {
	t.Helper()
	req := &ir.Request{Model: "m", ResponseFormat: rf,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockText, Text: "hi"},
		}}},
	}
	oc, ok := codec.Outbound(gemini.Name)
	if !ok {
		t.Fatal("gemini outbound 未注册")
	}
	lo, ok := oc.(codec.LossyEncoder)
	if !ok {
		t.Fatal("gemini 应实现 LossyEncoder")
	}
	raw, notes, err := lo.EncodeRequestLossy(req)
	if err != nil {
		t.Fatalf("EncodeRequestLossy: %v", err)
	}
	return string(raw), notes
}

func hasSubstr(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// 响应 schema 里被方言剔除的关键字要报出：此前 applyResponseFormat 静默削掉
// 约束，客户端会 JSON.parse 到一个可能违反 schema 的载荷却无从归因。
func TestResponseSchemaDroppedKeysReported(t *testing.T) {
	_, notes := respSchemaRequest(t, &ir.ResponseFormat{
		Kind:   ir.ResponseFormatSchema,
		Schema: `{"type":"object","properties":{"s":{"type":"string","minLength":1}}}`,
	})
	if !hasSubstr(notes, "response_format.schema keywords") {
		t.Errorf("缺响应 schema 剔除关键字注记：%#v", notes)
	}
	if !hasSubstr(notes, "minLength") {
		t.Errorf("注记未点名 minLength：%#v", notes)
	}
}

// 响应 schema 不是合法 JSON 时整体退成纯 JSON 模式，要报出降级。
func TestResponseSchemaInvalidJSONDowngradeReported(t *testing.T) {
	_, notes := respSchemaRequest(t, &ir.ResponseFormat{
		Kind:   ir.ResponseFormatSchema,
		Schema: `{not json`,
	})
	if !hasSubstr(notes, "downgraded to plain JSON mode") {
		t.Errorf("缺非法 JSON 降级注记：%#v", notes)
	}
}

// 响应 schema 顶层 properties 为空 → Omit，退成纯 JSON 模式，要报出。
func TestResponseSchemaOmitDowngradeReported(t *testing.T) {
	_, notes := respSchemaRequest(t, &ir.ResponseFormat{
		Kind:   ir.ResponseFormatSchema,
		Schema: `{"type":"object","properties":{}}`,
	})
	if !hasSubstr(notes, "downgraded to plain JSON mode") {
		t.Errorf("缺 Omit 降级注记：%#v", notes)
	}
}

// 干净、无需归一的响应 schema 不得误报任何 response_format.schema 注记。
func TestResponseSchemaCleanNoNote(t *testing.T) {
	_, notes := respSchemaRequest(t, &ir.ResponseFormat{
		Kind:   ir.ResponseFormatSchema,
		Schema: `{"type":"object","properties":{"a":{"type":"string"}}}`,
	})
	if hasSubstr(notes, "response_format.schema") {
		t.Errorf("干净 schema 被误报：%#v", notes)
	}
}

// 纯 JSON 模式（无 schema）不走归一，也不得报 schema 注记。
func TestResponseFormatPlainJSONNoSchemaNote(t *testing.T) {
	_, notes := respSchemaRequest(t, &ir.ResponseFormat{Kind: ir.ResponseFormatJSON})
	if hasSubstr(notes, "response_format.schema") {
		t.Errorf("纯 JSON 模式误报 schema 注记：%#v", notes)
	}
}
