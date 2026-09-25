package responses

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// created_at 保真：上游给过的创建时间同族往返不得被代理本地钟覆盖，
// 客户端按 created_at 做幂等/排序会拿到假数据。

func TestResponseCreatedAtRoundTrip(t *testing.T) {
	body := []byte(`{"id":"r1","object":"response","created_at":1700000000,"model":"m","status":"completed",` +
		`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`)
	resp, err := DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.Created != 1700000000 {
		t.Fatalf("上游 created_at 没落进 IR：%d", resp.Created)
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if !strings.Contains(string(out), `"created_at":1700000000`) {
		t.Errorf("出站 created_at 被本地钟覆盖：%s", out)
	}
}

// 上游没给创建时间（零值）时才回退本地钟：created_at 必须存在且非零。
func TestResponseCreatedAtFallback(t *testing.T) {
	resp, err := DecodeResponse([]byte(`{"id":"r1","model":"m","status":"completed","output":[]}`))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	s := string(out)
	if strings.Contains(s, `"created_at":0`) || !strings.Contains(s, `"created_at":`) {
		t.Errorf("缺省回退没写出有效 created_at：%s", s)
	}
}

// 流式往返：response.created 帧里的 created_at 随首帧进 IR，
// 编码侧每个 response 对象原值回写。
func TestStreamCreatedAtRoundTrip(t *testing.T) {
	evs := feedRaw(t,
		`{"type":"response.created","response":{"id":"r1","model":"m","created_at":1700000000}}`,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hi"}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed","created_at":1700000000}}`,
	)
	var start *ir.Event
	for i := range evs {
		if evs[i].Type == ir.EvMessageStart {
			start = &evs[i]
		}
	}
	if start == nil || start.Created != 1700000000 {
		t.Fatalf("created 帧的 created_at 没随首帧进 IR：%+v", evs)
	}

	s := encodeAll(t, evs...)
	if !strings.Contains(s, `"created_at":1700000000`) {
		t.Errorf("流式出站 created_at 被本地钟覆盖：\n%s", s)
	}
	if strings.Contains(s, `"created_at":0`) {
		t.Errorf("created_at 被写成零值：\n%s", s)
	}
}
