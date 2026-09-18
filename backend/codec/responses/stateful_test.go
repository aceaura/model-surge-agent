package responses

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 这三个字段把对话状态托管在上游那一侧。请求会被分发到任意一个目标账号，
// 那里没有这条 id 指向的历史；收下再忽略等于悄悄丢掉客户端以为已经带上的
// 上下文，模型会答得莫名其妙而没人知道为什么。
func TestStatefulFieldsAreRejected(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"previous_response_id", `{"model":"m","input":"hi","previous_response_id":"resp_1"}`, "previous_response_id"},
		{"conversation", `{"model":"m","input":"hi","conversation":"conv_1"}`, "conversation"},
		{"conversation object", `{"model":"m","input":"hi","conversation":{"id":"conv_1"}}`, "conversation"},
		{"prompt", `{"model":"m","input":"hi","prompt":{"id":"pmpt_1"}}`, "prompt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := DecodeRequest([]byte(c.body))
			if err == nil {
				t.Fatal("decode must reject a request that relies on upstream-held state")
			}
			irErr, ok := err.(*ir.Error)
			if !ok {
				t.Fatalf("err = %T, want *ir.Error", err)
			}
			if irErr.StatusCode != 400 {
				t.Errorf("status = %d, want 400", irErr.StatusCode)
			}
			if !strings.Contains(irErr.Message, c.want) {
				t.Errorf("message = %q, must name the offending field %q", irErr.Message, c.want)
			}
		})
	}
}

// 客户端往往同时带了两三个。一次只报一个会让它改一处再撞一次，白等一个来回。
func TestRejectionNamesEveryStatefulFieldPresent(t *testing.T) {
	body := `{"model":"m","input":"hi","previous_response_id":"resp_1",` +
		`"conversation":"conv_1","prompt":{"id":"pmpt_1"}}`
	_, err := DecodeRequest([]byte(body))
	if err == nil {
		t.Fatal("decode must reject the request")
	}
	for _, want := range []string{"previous_response_id", "conversation", "prompt"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message = %q, must name %q too", err.Error(), want)
		}
	}
}

// 显式的 null 等于没带这个字段，不该拒收一个其实合法的请求。
func TestExplicitNullStatefulFieldsPassThrough(t *testing.T) {
	body := `{"model":"m","input":"hi","conversation":null,"prompt":null}`
	if _, err := DecodeRequest([]byte(body)); err != nil {
		t.Fatalf("decode: %v, an explicit null carries no state", err)
	}
}
