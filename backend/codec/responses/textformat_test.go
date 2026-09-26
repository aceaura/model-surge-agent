package responses

import (
	"strings"
	"testing"
)

// 未知 text.format.type 必须 400，而不是静默解成 nil。
func TestUnknownTextFormatRejected(t *testing.T) {
	body := `{"model":"m","input":"hi","text":{"format":{"type":"bogus"}}}`
	if _, err := DecodeRequest([]byte(body)); err == nil {
		t.Fatalf("未知 text.format.type 应报错，却通过了")
	} else if !strings.Contains(err.Error(), "text.format") {
		t.Errorf("错误应指明 text.format 字段，实得 %v", err)
	}
}

// text 是默认形态、json_object / json_schema 是已知要求，都不该报错。
func TestKnownTextFormatsAccepted(t *testing.T) {
	for _, f := range []string{
		`{"type":"text"}`,
		`{"type":"json_object"}`,
		`{"type":"json_schema","name":"n","schema":{"type":"object"}}`,
	} {
		body := `{"model":"m","input":"hi","text":{"format":` + f + `}}`
		if _, err := DecodeRequest([]byte(body)); err != nil {
			t.Errorf("text.format=%s 不该报错: %v", f, err)
		}
	}
}
