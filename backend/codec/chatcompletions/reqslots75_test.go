package chatcompletions

import (
	"strings"
	"testing"
)

// #75 两字段在本协议（来源族）的往返保真：
// stream_options.include_obfuscation（三态，与恒写的 include_usage 共存）、
// response_format.json_schema.description（chat 嵌套形态）。

func TestIncludeObfuscationThreeState(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantNil  bool
		wantWire string
	}{
		{"显式 true", `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
			`"stream_options":{"include_obfuscation":true}}`, false, `"include_obfuscation":true`},
		{"显式 false", `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
			`"stream_options":{"include_obfuscation":false}}`, false, `"include_obfuscation":false`},
		{"没提", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, err := DecodeRequest([]byte(c.body))
			if err != nil {
				t.Fatal(err)
			}
			if c.wantNil && req.IncludeObfuscation != nil {
				t.Fatalf("应保持 nil，实得 %v", *req.IncludeObfuscation)
			}
			out, err := EncodeRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			s := string(out)
			// 出站恒写 include_usage（调度层要用量帧），三态都要在。
			if !strings.Contains(s, `"include_usage":true`) {
				t.Errorf("恒写的 include_usage 丢了：%s", s)
			}
			if c.wantNil {
				if strings.Contains(s, "include_obfuscation") {
					t.Errorf("没表态却造出键：%s", s)
				}
				return
			}
			if !strings.Contains(s, c.wantWire) {
				t.Errorf("回写丢了 %s：%s", c.wantWire, s)
			}
		})
	}
}

func TestJSONSchemaDescriptionRoundTrip(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"response_format":{"type":"json_schema","json_schema":` +
		`{"name":"n","description":"输出一个点","schema":{"type":"object"}}}}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.ResponseFormat == nil || req.ResponseFormat.Description != "输出一个点" {
		t.Fatalf("description 没收下：%+v", req.ResponseFormat)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"description":"输出一个点"`) {
		t.Errorf("回写丢了 description：%s", out)
	}
}

func TestJSONSchemaDescriptionAbsentStaysAbsent(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"response_format":{"type":"json_schema","json_schema":` +
		`{"name":"n","schema":{"type":"object"}}}}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "description") {
		t.Errorf("没给的 description 被凭空造出：%s", out)
	}
}
