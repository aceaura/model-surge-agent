package chatcompletions

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// #76-B8：messages[].name 贯通。此前 wireMessage 上字段声明了却无人读——
// 多方参与者身份（群聊/agent 编排里区分同名角色的不同实体）进不了 IR，
// 同族往返也丢。name 只有 chat 族有槽位：同族原样带回即保真口径。

func TestMessageNameRoundTrip(t *testing.T) {
	req, err := DecodeRequest([]byte(
		`{"model":"g","messages":[` +
			`{"role":"user","content":"hello","name":"alice"},` +
			`{"role":"assistant","content":"hi alice","name":"assistant-1"},` +
			`{"role":"user","content":[{"type":"text","text":"multi part"}],"name":"bob"}` +
			`]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("消息数不对：%d", len(req.Messages))
	}
	if req.Messages[0].Name != "alice" || req.Messages[1].Name != "assistant-1" ||
		req.Messages[2].Name != "bob" {
		t.Fatalf("name 没进 IR：%+v", req.Messages)
	}

	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, n := range []string{`"name":"alice"`, `"name":"assistant-1"`, `"name":"bob"`} {
		if !strings.Contains(s, n) {
			t.Errorf("回写缺 %s：%s", n, s)
		}
	}
}

// tool 角色消息的 name 同样入 IR：部分编排框架用它标注工具执行方。
func TestToolMessageNameEntersIR(t *testing.T) {
	req, err := DecodeRequest([]byte(
		`{"model":"g","messages":[` +
			`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},` +
			`{"role":"tool","tool_call_id":"c1","content":"ok","name":"executor-2"}` +
			`]}`))
	if err != nil {
		t.Fatal(err)
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Name != "executor-2" {
		t.Errorf("tool 消息的 name 丢了：%+v", last)
	}
}

// 无名消息不多键：缺省语义保持不变。
func TestMessageNameAbsentStaysAbsent(t *testing.T) {
	out, err := EncodeRequest(&ir.Request{
		Model:    "g",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `"name"`) {
		t.Errorf("无名消息发明了 name 键：%s", out)
	}
}
