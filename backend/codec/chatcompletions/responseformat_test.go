package chatcompletions

import (
	"strings"
	"testing"
)

// 未知 response_format.type 必须 400，而不是静默解成 nil（那会把「要求结构化
// 输出」悄悄降级成「不要求」，客户端拿到自由文本还以为约束生效）。
func TestUnknownResponseFormatRejected(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"response_format":{"type":"bogus"}}`)
	if _, err := DecodeRequest(body); err == nil {
		t.Fatalf("未知 response_format.type 应报错，却通过了")
	} else if !strings.Contains(err.Error(), "response_format") {
		t.Errorf("错误应指明 response_format 字段，实得 %v", err)
	}
}

// text 是默认形态、json_object / json_schema 是已知要求，都不该报错。
func TestKnownResponseFormatsAccepted(t *testing.T) {
	for _, rf := range []string{
		`{"type":"text"}`,
		`{"type":"json_object"}`,
		`{"type":"json_schema","json_schema":{"name":"n","schema":{"type":"object"}}}`,
	} {
		body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":` + rf + `}`)
		if _, err := DecodeRequest(body); err != nil {
			t.Errorf("response_format=%s 不该报错: %v", rf, err)
		}
	}
}
