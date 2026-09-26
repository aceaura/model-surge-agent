package anthropic

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// message_start 帧的 message 对象必须是完整的官方 Message 形状：
// type/content/stop_reason/stop_sequence 四键恒在，usage 的
// input_tokens/output_tokens 恒在。官方 SDK 按必填字段反序列化，
// 缺键或把 stop_reason 写成缺省而非显式 null，严格客户端会在流的
// 第一帧直接解析失败。
func TestMessageStartCarriesOfficialKeySet(t *testing.T) {
	enc := newStreamEncoder()
	frames, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "msg_1", Model: "claude-opus-5"})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("帧数 = %d，想要 1", len(frames))
	}
	var env struct {
		Type    string `json:"type"`
		Message map[string]json.RawMessage
	}
	raw := frames[0]
	i := bytes.Index(raw, []byte("data: "))
	if i < 0 {
		t.Fatalf("帧里没有 data 行：%s", raw)
	}
	data := bytes.TrimSpace(raw[i+len("data: "):])
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("帧不是合法 JSON（%v）：%s", err, frames[0])
	}
	if env.Type != "message_start" {
		t.Fatalf("外层 type = %q", env.Type)
	}
	for _, key := range []string{"type", "id", "role", "content", "stop_reason", "stop_sequence", "usage", "model"} {
		if _, ok := env.Message[key]; !ok {
			t.Errorf("message 缺官方键 %q：%s", key, frames[0])
		}
	}
	if string(env.Message["type"]) != `"message"` {
		t.Errorf("message.type = %s，想要 \"message\"", env.Message["type"])
	}
	if string(env.Message["content"]) != `[]` {
		t.Errorf("message.content = %s，想要空数组", env.Message["content"])
	}
	if string(env.Message["stop_reason"]) != `null` || string(env.Message["stop_sequence"]) != `null` {
		t.Errorf("stop 双键必须是显式 null：stop_reason=%s stop_sequence=%s",
			env.Message["stop_reason"], env.Message["stop_sequence"])
	}
	var usage map[string]json.RawMessage
	if err := json.Unmarshal(env.Message["usage"], &usage); err != nil {
		t.Fatalf("usage 不是对象：%v", err)
	}
	for _, key := range []string{"input_tokens", "output_tokens"} {
		if _, ok := usage[key]; !ok {
			t.Errorf("usage 缺必填键 %q：%s", key, env.Message["usage"])
		}
	}
}
