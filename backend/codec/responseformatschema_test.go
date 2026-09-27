package codec

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 客户端选了 json_schema 却没带 schema 原文（轮次12 F2）：OpenAI 两系的解码器
// 把 {"type":"json_schema"}（无 json_schema 子对象）解成 Kind=Schema、Schema 空。
// 没有结构可约束时——
//   - chat_completions/responses 退回 {type:"json_object"}、gemini 写
//     responseMimeType="application/json"，都保住了「至少是 JSON」这一最低要求，
//     不算丢失，不得报；
//   - anthropic 的 output_config.format 只接 json_schema 一种 type，退回不了纯
//     JSON（ResponseSchema 真而 ResponseFormat 假），空 schema 时编码器什么都写不
//     出（encode_request.go 要求 Schema != ""），连「要 JSON」都落空，响应变自由
//     文本，必须报。此前 case caps.ResponseSchema 分支对此完全沉默，还注释「原样
//     送达」——是假阴性。判据锚定 rf.Schema == "" && !caps.ResponseFormat，与
//     纯 JSON 模式在 anthropic 上的处置（else 分支）同口径。

func schemaReq(schema string) *ir.Request {
	return &ir.Request{
		Model:     "m",
		MaxTokens: 16,
		ResponseFormat: &ir.ResponseFormat{
			Kind:   ir.ResponseFormatSchema,
			Schema: schema,
		},
	}
}

// schemaLossy 用**真实**出站能力位跑诊断，端到端验证 caps 接线，而非手搓 caps。
func schemaLossy(t *testing.T, name string, req *ir.Request) string {
	t.Helper()
	oc, ok := Outbound(name)
	if !ok {
		t.Fatalf("Outbound(%s) 不可用", name)
	}
	return strings.Join(DescribeLossy(req, name, oc.Caps()), "\n")
}

const emptySchemaNote = "carried no schema"

// anthropic 空 schema：JSON 要求整体落空，必须报。
func TestDescribeLossyEmptySchemaNotedForAnthropic(t *testing.T) {
	got := schemaLossy(t, ProtocolAnthropic, schemaReq(""))
	if !strings.Contains(got, emptySchemaNote) {
		t.Errorf("anthropic 收到空 schema 的 json_schema 请求应报 JSON 要求落空:\n%s", got)
	}
}

// anthropic 带 schema：约束原样送达，不得报。
func TestDescribeLossyNonEmptySchemaSilentForAnthropic(t *testing.T) {
	got := schemaLossy(t, ProtocolAnthropic, schemaReq(`{"type":"object"}`))
	if strings.Contains(got, emptySchemaNote) {
		t.Errorf("anthropic 收到带 schema 的请求被误报为空 schema:\n%s", got)
	}
}

// 能退回纯 JSON 的三家（chat/responses/gemini）：空 schema 不算丢失，不得报。
func TestDescribeLossyEmptySchemaSilentForJSONFallbackTargets(t *testing.T) {
	for _, name := range []string{ProtocolChatCompletions, ProtocolResponses, ProtocolGemini} {
		got := schemaLossy(t, name, schemaReq(""))
		if strings.Contains(got, emptySchemaNote) {
			t.Errorf("%s 能退回纯 JSON，空 schema 不该报落空:\n%s", name, got)
		}
	}
}

// 事实出处对齐：只有 anthropic 是 ResponseSchema 真而 ResponseFormat 假的目标，
// 故只有它在空 schema 时落空。这条钉住能力位接线，防止日后某家翻转 ResponseFormat
// 后诊断与编码器行为漂移。
func TestOnlyAnthropicLacksPlainJSONFallback(t *testing.T) {
	want := map[string]bool{ // 值 = 是否「只收 schema、退回不了纯 JSON」
		ProtocolAnthropic:       true,
		ProtocolChatCompletions: false,
		ProtocolResponses:       false,
		ProtocolGemini:          false,
	}
	for name, schemaOnly := range want {
		oc, ok := Outbound(name)
		if !ok {
			t.Fatalf("Outbound(%s) 不可用", name)
		}
		caps := oc.Caps()
		if got := caps.ResponseSchema && !caps.ResponseFormat; got != schemaOnly {
			t.Errorf("%s (ResponseSchema && !ResponseFormat)=%v，应为 %v", name, got, schemaOnly)
		}
	}
}
