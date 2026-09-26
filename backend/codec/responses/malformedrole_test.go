package responses

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// responses 入站解码的畸形输入纪律（轮次5 F1/F3）。refusal 是 assistant 专属的
// 输出槽位、消息角色名必须已知——此前两者都被静默改写：refusal 落到 user 消息
// 会被 chat 编码器悄悄丢弃（refusal 只在 assistant 消息回写）且不留注记，落到
// system 又会在外族编码器整轮报错；未知角色一律当成 user，客户端拼错角色名时
// 消息以 user 身份送进模型、答非所问却报成功。按「畸形请求→400」纪律，在唯一
// 的入站解码点一次拒清，而不是让下游各处各自补救。

// decodeErrStatus 解码 body 并要求它返回一个 *ir.Error，回传该错误与其消息。
func decodeErrStatus(t *testing.T, body string) (*ir.Error, string) {
	t.Helper()
	_, err := DecodeRequest([]byte(body))
	if err == nil {
		t.Fatalf("expected an error, got nil for body: %s", body)
	}
	ire, ok := err.(*ir.Error)
	if !ok {
		t.Fatalf("err = %T (%v), want *ir.Error", err, err)
	}
	return ire, err.Error()
}

// 非 assistant 消息携带 refusal 部件 → 400，不得静默放行。
func TestDecodeRequestRejectsStrayRefusal(t *testing.T) {
	for _, role := range []string{"user", "system", "developer"} {
		t.Run(role, func(t *testing.T) {
			body := `{"model":"m","input":[{"type":"message","role":"` + role +
				`","content":[{"type":"refusal","refusal":"I cannot help"}]}]}`
			ire, msg := decodeErrStatus(t, body)
			if ire.StatusCode != 400 {
				t.Errorf("status = %d, want 400", ire.StatusCode)
			}
			if !strings.Contains(msg, "refusal content part is only valid in an assistant message") {
				t.Errorf("message = %q, want the stray-refusal rejection", msg)
			}
		})
	}
}

// 反向护栏：assistant 消息里的 refusal 是合法历史，必须原样解成 BlockRefusal，
// 纠正畸形输入不得顺手拒掉正当用法。
func TestDecodeRequestKeepsAssistantRefusal(t *testing.T) {
	body := `{"model":"m","input":[{"type":"message","role":"assistant",` +
		`"content":[{"type":"refusal","refusal":"I cannot help"}]}]}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("assistant refusal should decode, got: %v", err)
	}
	var found bool
	for _, m := range req.Messages {
		if m.Role != ir.RoleAssistant {
			t.Errorf("role = %q, want assistant", m.Role)
		}
		for _, b := range m.Content {
			if b.Type == ir.BlockRefusal && b.Text == "I cannot help" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("assistant refusal not decoded into a BlockRefusal: %+v", req.Messages)
	}
}

// 未知角色 → 400，不再静默当成 user。大小写敏感（"User" 也是未知）。
func TestDecodeRequestRejectsUnknownRole(t *testing.T) {
	for _, role := range []string{"tool", "function", "narrator", "User"} {
		t.Run(role, func(t *testing.T) {
			body := `{"model":"m","input":[{"type":"message","role":"` + role +
				`","content":[{"type":"input_text","text":"hi"}]}]}`
			ire, msg := decodeErrStatus(t, body)
			if ire.StatusCode != 400 {
				t.Errorf("status = %d, want 400", ire.StatusCode)
			}
			if !strings.Contains(msg, "unknown role") {
				t.Errorf("message = %q, want the unknown-role rejection", msg)
			}
		})
	}
}

// 反向护栏：已知角色、空角色（按 user 处理，与 chat 解码器同口径）、以及省略
// type 靠 role 推断的裸消息都必须照常解码。
func TestDecodeRequestAcceptsKnownRoles(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"user", `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`},
		{"assistant", `{"model":"m","input":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`},
		{"system", `{"model":"m","input":[{"type":"message","role":"system","content":[{"type":"input_text","text":"be brief"}]}]}`},
		{"developer", `{"model":"m","input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"be brief"}]}]}`},
		{"empty role defaults to user", `{"model":"m","input":[{"type":"message","role":"","content":[{"type":"input_text","text":"hi"}]}]}`},
		{"no type infers message", `{"model":"m","input":[{"role":"user","content":"hi"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeRequest([]byte(tc.body)); err != nil {
				t.Fatalf("known/empty role should decode, got: %v", err)
			}
		})
	}
}
