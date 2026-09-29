package codec_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	_ "github.com/aceaura/model-surge-agent/backend/codec/anthropic"
	_ "github.com/aceaura/model-surge-agent/backend/codec/chatcompletions"
	_ "github.com/aceaura/model-surge-agent/backend/codec/gemini"
	_ "github.com/aceaura/model-surge-agent/backend/codec/responses"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// custom 指名变体的原文形状逐族不同：chat 是嵌套 {"type":"custom","custom":
// {"name":…}}，responses 是扁平 {"type":"custom","name":…}。Raw 只能由产出它
// 的那一族逐字回写（RawFamily == 本族 Name）；外族必须落到结构化分支按
// Mode/Name 重编，否则会把对方解析不了的键漏进请求体（chat 的嵌套 custom 送进
// responses、responses 的扁平 custom 送进 chat 都会挨 400）。
//
// 请求体里除 tool_choice 外不含 "custom" 子串（工具是普通函数 grep、schema 是
// object/properties），因此「body 是否含 custom」是 Raw 泄漏的干净探针。
func namedCustomChoiceRequest(raw, family string) *ir.Request {
	return &ir.Request{
		Model: "m", MaxTokens: 16,
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
		},
		Tools: []ir.Tool{{Name: "grep", Schema: `{"type":"object","properties":{}}`}},
		ToolChoice: &ir.ToolChoice{
			Mode: ir.ToolChoiceTool, Name: "grep",
			Raw: json.RawMessage(raw), RawFamily: family,
		},
	}
}

func TestCustomToolChoiceRawNeverLeaksCrossFamily(t *testing.T) {
	cases := []struct {
		home     string // 产出 Raw 的族
		raw      string // 该族的 custom 原文
		verbatim string // 同族回写应逐字出现的片段
		homeName string // 同族协议名
	}{
		{
			home:     "chat 嵌套 custom",
			raw:      `{"type":"custom","custom":{"name":"grep"}}`,
			verbatim: `"tool_choice":{"type":"custom","custom":{"name":"grep"}}`,
			homeName: codec.ProtocolChatCompletions,
		},
		{
			home:     "responses 扁平 custom",
			raw:      `{"type":"custom","name":"grep"}`,
			verbatim: `"tool_choice":{"type":"custom","name":"grep"}`,
			homeName: codec.ProtocolResponses,
		},
	}
	for _, c := range cases {
		req := namedCustomChoiceRequest(c.raw, c.homeName)
		for _, name := range codec.OutboundNames() {
			oc, _ := codec.Outbound(name)
			body, err := oc.EncodeRequest(req.Clone())
			if err != nil {
				t.Fatalf("%s → %s EncodeRequest: %v", c.home, name, err)
			}
			s := string(body)
			if name == c.homeName {
				// 同族：逐字回写，custom 形状必须原样出现。
				if !strings.Contains(s, c.verbatim) {
					t.Errorf("%s → %s 同族没逐字回写 custom：%s", c.home, name, s)
				}
				continue
			}
			// 外族：Raw 不得泄漏，必须结构化重编成具名工具（不含 custom 子串）。
			if strings.Contains(s, "custom") {
				t.Errorf("%s → %s 把本族 custom 原文漏进了外族请求体：%s", c.home, name, s)
			}
			if !strings.Contains(s, "grep") {
				t.Errorf("%s → %s 外族结构化重编丢了工具名：%s", c.home, name, s)
			}
		}
	}
}
