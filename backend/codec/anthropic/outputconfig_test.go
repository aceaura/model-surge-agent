package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// output_config.format（2026 新增）是本协议唯一的结构化输出槽位，且只接
// json_schema 一种 type：schema 约束原样往返，纯 JSON 模式（只要求合法 JSON、
// 不给 schema）没有落点，由诊断报受限（见 codec/paramfidelity_test.go）。
//
// 对应旧仓 #22（bcc67c8）。

const ocSchema = `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`

// 解码：json_schema 形态进 IR，恒为严格语义（没有 strict 开关也没有名称位）。
func TestDecodeOutputConfigFormat(t *testing.T) {
	body := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],` +
		`"output_config":{"format":{"type":"json_schema","schema":` + ocSchema + `}}}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	rf := req.ResponseFormat
	if rf == nil {
		t.Fatal("output_config.format 没进 IR")
	}
	if rf.Kind != ir.ResponseFormatSchema {
		t.Errorf("Kind = %q, want schema", rf.Kind)
	}
	if rf.Schema != ocSchema {
		t.Errorf("Schema = %q, want %q", rf.Schema, ocSchema)
	}
	if rf.Name != "" {
		t.Errorf("本协议没有名称位，Name = %q", rf.Name)
	}
	if rf.Strict == nil || !*rf.Strict {
		t.Errorf("json_schema 恒为严格语义，Strict = %v", rf.Strict)
	}
}

// 非 json_schema 的 type、空 schema、null schema、以及只有 effort 的
// output_config 都按没给处理：空约束写出来上游也是自由文本，不发明诉求。
func TestDecodeIgnoresUnusableOutputConfig(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"unknown-type", `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],"output_config":{"format":{"type":"json_mode","schema":` + ocSchema + `}}}`},
		{"empty-schema", `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],"output_config":{"format":{"type":"json_schema"}}}`},
		{"null-schema", `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],"output_config":{"format":{"type":"json_schema","schema":null}}}`},
		{"effort-only", `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],"output_config":{"effort":"high"}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			req, err := DecodeRequest([]byte(c.body))
			if err != nil {
				t.Fatalf("DecodeRequest: %v", err)
			}
			if req.ResponseFormat != nil {
				t.Errorf("%s 不该进 IR：%+v", c.name, req.ResponseFormat)
			}
		})
	}
}

// 编码：schema 约束写进 output_config.format，type 恒 json_schema；
// OpenAI 形态的 name/strict 键没有槽位，不得带过去。
func TestEncodeOutputConfigFormat(t *testing.T) {
	strict := true
	req := &ir.Request{
		Model: "m", MaxTokens: 10,
		ResponseFormat: &ir.ResponseFormat{
			Kind: ir.ResponseFormatSchema, Name: "weather", Schema: ocSchema, Strict: &strict,
		},
	}
	body, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	var got wireRequest
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, body)
	}
	if got.OutputConfig == nil || got.OutputConfig.Format == nil {
		t.Fatalf("output_config 没写出去：%s", body)
	}
	if got.OutputConfig.Format.Type != "json_schema" {
		t.Errorf("type = %q, want json_schema", got.OutputConfig.Format.Type)
	}
	if string(got.OutputConfig.Format.Schema) != ocSchema {
		t.Errorf("schema 本体变了：%s", got.OutputConfig.Format.Schema)
	}
	if strings.Contains(string(body), `"strict"`) || strings.Contains(string(body), `"name":"weather"`) {
		t.Errorf("把 OpenAI 形态字段写进了 anthropic 载荷：%s", body)
	}
}

// 纯 JSON 模式（没给 schema）在 anthropic 没有对应物：一个字节都不写，
// 由诊断层报受限（ResponseSchema 真而 ResponseFormat 假）。
func TestEncodeOmitsJSONModeWithoutSchema(t *testing.T) {
	req := &ir.Request{
		Model: "m", MaxTokens: 10,
		ResponseFormat: &ir.ResponseFormat{Kind: ir.ResponseFormatJSON},
	}
	body, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if strings.Contains(string(body), "output_config") {
		t.Errorf("纯 JSON 模式不该写出 output_config：%s", body)
	}
}

// schema 形态但 Schema 串为空：同样不写——空约束不是诉求。
func TestEncodeOmitsSchemaWithoutBody(t *testing.T) {
	req := &ir.Request{
		Model: "m", MaxTokens: 10,
		ResponseFormat: &ir.ResponseFormat{Kind: ir.ResponseFormatSchema},
	}
	body, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if strings.Contains(string(body), "output_config") {
		t.Errorf("空 schema 不该写出 output_config：%s", body)
	}
}

// 同族往返：anthropic 入站的 schema 经 anthropic 出站再回解，约束不丢。
func TestOutputConfigSameFamilyRoundTrip(t *testing.T) {
	in := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],` +
		`"output_config":{"format":{"type":"json_schema","schema":` + ocSchema + `}}}`
	req, err := DecodeRequest([]byte(in))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	back, err := DecodeRequest(out)
	if err != nil {
		t.Fatalf("回解: %v", err)
	}
	if back.ResponseFormat == nil || back.ResponseFormat.Kind != ir.ResponseFormatSchema {
		t.Fatalf("往返后约束丢了：%+v", back.ResponseFormat)
	}
	if back.ResponseFormat.Schema != ocSchema {
		t.Errorf("往返后 schema 变了：%q", back.ResponseFormat.Schema)
	}
}
