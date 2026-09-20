package codec_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 这条是本轮的入口证据：单条只含无人应答 tool_use 的 assistant 消息经
// Sanitize 后一条不剩，四个出站协议原来分别发出 messages:null / contents:null
// / input:[]，上游全报字段缺失或类型错的 400。归因还指向我们改写之后，
// 客户端拿不到原因。
func TestSanitizedToEmptyStillProducesAValidBody(t *testing.T) {
	req := &ir.Request{Model: "m", Messages: []ir.Message{
		{Role: ir.RoleAssistant, Content: []ir.Block{
			{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "t1", Name: "f", Input: `{}`}},
		}},
	}}
	ir.Sanitize(req)
	if len(req.Messages) != 0 {
		t.Fatalf("前置条件不成立：Sanitize 后还剩 %d 条消息，这条断言就测不到空序列", len(req.Messages))
	}
	for _, name := range outboundNames() {
		t.Run(name, func(t *testing.T) {
			oc, ok := codec.Outbound(name)
			if !ok {
				t.Fatalf("outbound %q not registered", name)
			}
			body, err := oc.EncodeRequest(req)
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			assertNonEmptyTurnList(t, name, body)
			if !strings.Contains(string(body), codec.ConversationPlaceholder) {
				t.Errorf("占位消息未出现在请求体里: %s", body)
			}
		})
	}
}

// 只有 system 的请求同样触发：客户端给了 system 却没给任何一轮对话是合法入站，
// 不能因此 400。
func TestSystemOnlyRequestStillProducesATurn(t *testing.T) {
	for _, name := range outboundNames() {
		t.Run(name, func(t *testing.T) {
			req := &ir.Request{Model: "m", System: []ir.Block{
				{Type: ir.BlockText, Text: "sys"},
			}}
			oc, _ := codec.Outbound(name)
			body, err := oc.EncodeRequest(req)
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			assertNonEmptyTurnList(t, name, body)
		})
	}
}

// 补位是有损的：客户端发的和模型看到的不一样，调用方要能看见。
func TestFilledPlaceholderIsReportedAsLossy(t *testing.T) {
	for _, name := range outboundNames() {
		t.Run(name, func(t *testing.T) {
			oc, _ := codec.Outbound(name)
			lo, ok := oc.(codec.LossyEncoder)
			if !ok {
				t.Skipf("%s 没有有损说明出口", name)
			}
			_, notes, err := lo.EncodeRequestLossy(&ir.Request{Model: "m"})
			if err != nil {
				t.Fatalf("EncodeRequestLossy: %v", err)
			}
			if !anyNoteMentions(notes, "message list is empty") {
				t.Errorf("补位未报进有损说明: %v", notes)
			}
		})
	}
}

// 有消息时不许无条件插占位：那会改变正常请求的前缀，打掉上游的 prompt cache。
func TestPlaceholderOnlyAppearsWhenTheListIsEmpty(t *testing.T) {
	for _, name := range outboundNames() {
		t.Run(name, func(t *testing.T) {
			req := &ir.Request{Model: "m", Messages: []ir.Message{
				{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
			}}
			oc, _ := codec.Outbound(name)
			body, err := oc.EncodeRequest(req)
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			if strings.Contains(string(body), codec.ConversationPlaceholder) {
				t.Errorf("首条已是 user 却插了占位: %s", body)
			}
		})
	}
}

// 整形在副本上做：调用方那份请求要留着换目标重试，被补位改写会让下一跳
// 看到一条它没发过的消息，且第二次补位还会再叠一条。
func TestPlaceholderDoesNotMutateTheCallersRequest(t *testing.T) {
	for _, name := range outboundNames() {
		t.Run(name, func(t *testing.T) {
			req := &ir.Request{Model: "m"}
			oc, _ := codec.Outbound(name)
			if _, err := oc.EncodeRequest(req); err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			if len(req.Messages) != 0 {
				t.Errorf("调用方的请求被改写了：多出 %d 条消息", len(req.Messages))
			}
		})
	}
}

// 补位那条必须是 user：assistant 占位在 anthropic 那边会变成连续两条
// assistant（前导占位 + 这条），而「对话须由 user 起头」这条要求本身也没
// 被满足。补一条模型自己说过的话还会让它以为上一轮已经答过了。
func TestFilledPlaceholderIsAUserTurn(t *testing.T) {
	shaped := &ir.Request{Model: "m"}
	codec.ShapeRequest(shaped, "probe", codec.Capabilities{})
	if len(shaped.Messages) != 1 {
		t.Fatalf("消息数 = %d，想要 1 条补位", len(shaped.Messages))
	}
	if shaped.Messages[0].Role != ir.RoleUser {
		t.Fatalf("补位消息的角色 = %q，想要 user", shaped.Messages[0].Role)
	}
	for _, name := range outboundNames() {
		t.Run(name, func(t *testing.T) {
			oc, _ := codec.Outbound(name)
			body, err := oc.EncodeRequest(&ir.Request{Model: "m"})
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			assertPlaceholderSitsInAUserTurn(t, name, body)
		})
	}
}

// assertPlaceholderSitsInAUserTurn 断言占位文本所在的那一轮的角色是 user。
//
// 逐轮解出来判角色而不是搜 `"role":"user"`：请求体里本来就可能有别的 user
// 轮，搜字符串会被它们骗过。gemini 的角色字段名是 role 但取值 user/model，
// responses 的输入项同样带 role，所以四个协议能共用一次解析。
func assertPlaceholderSitsInAUserTurn(t *testing.T, name string, body []byte) {
	t.Helper()
	field := turnListField(t, name)
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		t.Fatalf("请求体不是 JSON 对象: %v", err)
	}
	var list []json.RawMessage
	if err := json.Unmarshal(probe[field], &list); err != nil {
		t.Fatalf("%q 不是数组: %s", field, probe[field])
	}
	found := false
	for _, raw := range list {
		if !strings.Contains(string(raw), codec.ConversationPlaceholder) {
			continue
		}
		found = true
		var turn struct{ Role string }
		if err := json.Unmarshal(raw, &turn); err != nil {
			t.Fatalf("对话轮不是对象: %s", raw)
		}
		if turn.Role != "user" {
			t.Errorf("占位所在轮的角色 = %q，想要 user: %s", turn.Role, raw)
		}
	}
	if !found {
		t.Fatalf("占位文本不在任何一轮里: %s", body)
	}
}

// assertNonEmptyTurnList 断言请求体里的对话序列既不是 null 也不是空数组。
//
// 按协议取不同的字段名而不是只搜字符串：contents:null 与 messages:null 是
// 同一个缺陷的两种写法，而搜 "null" 会被请求体里任何一个别的 null 骗过。
func assertNonEmptyTurnList(t *testing.T, name string, body []byte) {
	t.Helper()
	field := turnListField(t, name)
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		t.Fatalf("请求体不是 JSON 对象: %v", err)
	}
	raw, ok := probe[field]
	if !ok {
		t.Fatalf("请求体里没有 %q 字段: %s", field, body)
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("%q 不是数组（很可能是 null）: %s", field, raw)
	}
	if len(list) == 0 {
		t.Fatalf("%q 是空数组，上游会拒: %s", field, body)
	}
}

// turnListField 给出各协议装对话序列的字段名。新增出站协议必须在此补齐，
// 否则这一组断言会静默地什么都不测。
func turnListField(t *testing.T, name string) string {
	t.Helper()
	field := map[string]string{
		codec.ProtocolAnthropic:       "messages",
		codec.ProtocolChatCompletions: "messages",
		codec.ProtocolResponses:       "input",
		codec.ProtocolGemini:          "contents",
	}[name]
	if field == "" {
		t.Fatalf("协议 %q 未登记它的对话序列字段名——新增出站协议必须在此补齐", name)
	}
	return field
}

func anyNoteMentions(notes []string, substr string) bool {
	for _, n := range notes {
		if strings.Contains(n, substr) {
			return true
		}
	}
	return false
}
