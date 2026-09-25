package responses

import (
	"strings"
	"testing"
)

// #75 三字段在本协议（来源族）的往返保真：max_tool_calls、
// stream_options.include_obfuscation（三态）、text.format.description。
// 此前 wireRequest 连槽位都没有：同族往返也静默蒸发。

func TestMaxToolCallsRoundTrip(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","max_tool_calls":4}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.MaxToolCalls == nil || *req.MaxToolCalls != 4 {
		t.Fatalf("max_tool_calls 没收下：%v", req.MaxToolCalls)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"max_tool_calls":4`) {
		t.Errorf("回写丢了 max_tool_calls：%s", out)
	}
}

func TestMaxToolCallsAbsentStaysAbsent(t *testing.T) {
	req, err := DecodeRequest([]byte(`{"model":"m","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "max_tool_calls") {
		t.Errorf("没给的 max_tool_calls 被凭空造出：%s", out)
	}
}

// 混淆开关三态：显式 false 是「关掉上游默认的混淆保护」，与没提不同，
// 两态布尔会把显式 false 吞回缺省。
func TestIncludeObfuscationThreeState(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantNil  bool
		wantWire string // 非 wantNil 时出站必须出现的片段
	}{
		{"显式 true", `{"model":"m","input":"hi","stream_options":{"include_obfuscation":true}}`,
			false, `"include_obfuscation":true`},
		{"显式 false", `{"model":"m","input":"hi","stream_options":{"include_obfuscation":false}}`,
			false, `"include_obfuscation":false`},
		{"没提", `{"model":"m","input":"hi"}`, true, ""},
		{"给了 stream_options 但没给键", `{"model":"m","input":"hi","stream_options":{}}`, true, ""},
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
			if c.wantNil {
				if strings.Contains(s, "include_obfuscation") || strings.Contains(s, "stream_options") {
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

// description 与 chat 的 json_schema.description 同键同义，本族平铺在
// text.format 下。
func TestTextFormatDescriptionRoundTrip(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","text":{"format":` +
		`{"type":"json_schema","name":"n","description":"输出一个点","schema":{"type":"object"}}}}`)
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

func TestTextFormatDescriptionAbsentStaysAbsent(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","text":{"format":` +
		`{"type":"json_schema","name":"n","schema":{"type":"object"}}}}`)
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
