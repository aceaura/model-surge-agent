package responses

import (
	"reflect"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// allowed_tools 是 responses/codex 一族的白名单形态，此前对象分支只认
// name，这个形态没有 name，一律 400（"name is required"）——客户端写明
// 「只能调这些」，得到一个参数错误，连注记都没有。现在解成 Mode +
// AllowedTools，落地靠整形阶段的收窄（见 codec/shape.go）。
func TestAllowedToolsToolChoiceDecode(t *testing.T) {
	cases := []struct {
		name string
		tc   string
		want ir.ToolChoice
	}{
		{"缺省 auto", `{"type":"allowed_tools","tools":[{"type":"function","name":"alpha"}]}`,
			ir.ToolChoice{Mode: ir.ToolChoiceAuto, AllowedTools: []string{"alpha"}}},
		{"required 折成 any", `{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"alpha"},{"type":"function","name":"beta"}]}`,
			ir.ToolChoice{Mode: ir.ToolChoiceAny, AllowedTools: []string{"alpha", "beta"}}},
		{"嵌套 function.name", `{"type":"allowed_tools","tools":[{"function":{"name":"alpha"}}]}`,
			ir.ToolChoice{Mode: ir.ToolChoiceAuto, AllowedTools: []string{"alpha"}}},
		{"裸字符串", `{"type":"allowed_tools","tools":["alpha","beta"]}`,
			ir.ToolChoice{Mode: ir.ToolChoiceAuto, AllowedTools: []string{"alpha", "beta"}}},
		{"认不出的条目跳过", `{"type":"allowed_tools","tools":["alpha",42,{"type":"image_generator"}]}`,
			ir.ToolChoice{Mode: ir.ToolChoiceAuto, AllowedTools: []string{"alpha"}}},
		{"tools 缺席", `{"type":"allowed_tools"}`,
			ir.ToolChoice{Mode: ir.ToolChoiceAuto}},
	}
	for _, c := range cases {
		body := `{"model":"m","input":"hi","tool_choice":` + c.tc + `}`
		req, err := DecodeRequest([]byte(body))
		if err != nil {
			t.Fatalf("%s: DecodeRequest: %v", c.name, err)
		}
		if req.ToolChoice == nil {
			t.Fatalf("%s: tool_choice 被整条丢掉", c.name)
		}
		if !reflect.DeepEqual(*req.ToolChoice, c.want) {
			t.Errorf("%s: = %+v，想要 %+v", c.name, *req.ToolChoice, c.want)
		}
	}
}

// 指名对象缺 name 仍然 400：放行的是白名单形态，不是放松具名校验。
func TestNamelessFunctionChoiceStillRejected(t *testing.T) {
	body := `{"model":"m","input":"hi","tool_choice":{"type":"function"}}`
	if _, err := DecodeRequest([]byte(body)); err == nil {
		t.Fatal("缺 name 的指名对象应被拒")
	}
}

func toolFixture() *ir.Request {
	return &ir.Request{
		Model: "m", MaxTokens: 64,
		Messages: []ir.Message{{Role: ir.RoleUser,
			Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		Tools: []ir.Tool{
			{Name: "alpha", Schema: `{"type":"object","properties":{}}`},
			{Name: "beta", Schema: `{"type":"object","properties":{}}`},
		},
	}
}

// 同族端到端：EncodeRequestLossy 走 ShapeRequest，白名单把工具列表收窄，
// tool_choice 落回普通字符串形态。收窄成功即无损，不该带注记。
func TestAllowedToolsNarrowOnEncode(t *testing.T) {
	req := toolFixture()
	req.ToolChoice = &ir.ToolChoice{Mode: ir.ToolChoiceAny, AllowedTools: []string{"alpha"}}
	body, notes, err := outboundCodec{}.EncodeRequestLossy(req)
	if err != nil {
		t.Fatalf("EncodeRequestLossy: %v", err)
	}
	s := string(body)
	if !strings.Contains(s, `"alpha"`) || strings.Contains(s, `"beta"`) {
		t.Errorf("白名单没收窄工具列表：%s", s)
	}
	if !strings.Contains(s, `"tool_choice":"required"`) {
		t.Errorf("tool_choice 应保留 required 档：%s", s)
	}
	for _, n := range notes {
		if strings.Contains(n, "allowlist") {
			t.Errorf("收窄成功不该报注记：%v", notes)
		}
	}
}

// 交集全空时无从收窄：整个工具列表照发（被禁的工具递到了模型面前），
// 必须报出——这是白名单唯一产生损耗的路径。
func TestUnenforceableAllowlistReported(t *testing.T) {
	req := toolFixture()
	req.ToolChoice = &ir.ToolChoice{Mode: ir.ToolChoiceAuto, AllowedTools: []string{"ghost"}}
	body, notes, err := outboundCodec{}.EncodeRequestLossy(req)
	if err != nil {
		t.Fatalf("EncodeRequestLossy: %v", err)
	}
	s := string(body)
	if !strings.Contains(s, `"alpha"`) || !strings.Contains(s, `"beta"`) {
		t.Errorf("无从收窄时应照发整个列表：%s", s)
	}
	joined := strings.Join(notes, "; ")
	if !strings.Contains(joined, "could not enforce the tool allowlist of 1 name(s)") {
		t.Errorf("未报白名单失效：%v", notes)
	}
}
