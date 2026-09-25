package chatcompletions

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// created 保真：上游给过的创建时间同族往返不得被代理本地钟覆盖，
// 客户端按 created 做幂等/排序会拿到假数据。

func TestResponseCreatedRoundTrip(t *testing.T) {
	body := []byte(`{"id":"c1","object":"chat.completion","created":1700000000,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	resp, _, err := DecodeResponseLossy(body)
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	if resp.Created != 1700000000 {
		t.Fatalf("上游 created 没落进 IR：%d", resp.Created)
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if !strings.Contains(string(out), `"created":1700000000`) {
		t.Errorf("出站 created 被本地钟覆盖：%s", out)
	}
}

// 上游没给创建时间（零值）时才回退本地钟：created 必须存在且非零。
func TestResponseCreatedFallback(t *testing.T) {
	resp, _, err := DecodeResponseLossy([]byte(
		`{"choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}]}`))
	if err != nil {
		t.Fatalf("DecodeResponseLossy: %v", err)
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	s := string(out)
	if strings.Contains(s, `"created":0`) || !strings.Contains(s, `"created":`) {
		t.Errorf("缺省回退没写出有效 created：%s", s)
	}
}

// 流式往返：chunk 里的 created 随首帧进 IR，编码侧逐帧原值回写。
func TestStreamCreatedRoundTrip(t *testing.T) {
	dec := newStreamDecoder()
	evs, err := dec.Feed("", `{"id":"c1","object":"chat.completion.chunk","created":1700000000,"model":"m",`+
		`"choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	var start *ir.Event
	for i := range evs {
		if evs[i].Type == ir.EvMessageStart {
			start = &evs[i]
		}
	}
	if start == nil || start.Created != 1700000000 {
		t.Fatalf("chunk 的 created 没随首帧进 IR：%+v", evs)
	}

	enc := newStreamEncoder()
	var out [][]byte
	for _, ev := range evs {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("Encode(%s): %v", ev.Type, err)
		}
		out = append(out, frames...)
	}
	out = append(out, enc.Finish()...)
	var joined []byte
	for _, f := range out {
		joined = append(joined, f...)
	}
	if !strings.Contains(string(joined), `"created":1700000000`) {
		t.Errorf("流式出站 created 被本地钟覆盖：%s", joined)
	}
	if strings.Contains(string(joined), `"created":0`) {
		t.Errorf("created 被写成零值：%s", joined)
	}
}
