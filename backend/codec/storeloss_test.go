package codec_test

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	_ "github.com/aceaura/model-surge-agent/backend/codec/anthropic"
	_ "github.com/aceaura/model-surge-agent/backend/codec/chatcompletions"
	_ "github.com/aceaura/model-surge-agent/backend/codec/gemini"
	_ "github.com/aceaura/model-surge-agent/backend/codec/responses"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// OpenAI 两系的 store 字段此前在 DTO 里没声明（chat）或声明了却不读
//（responses），客户端给的值静默蒸发：它以为这次响应存在上游、稍后可按 id
// 取回或链式引用，实际本服务对上游恒不留存（responses 出站强制 store:false，
// 其余三族连槽位都没有），且没有任何痕迹可查。收进 IR 后出站一律不回写为
// true，显式 true 由 DescribeLossy 报出——与 background 同款判据。

// 解码可见性：两族三态各自归位，缺席不发明。
func TestStoreDecodeIntoIR(t *testing.T) {
	cases := []struct {
		proto string
		body  string
	}{
		{codec.ProtocolResponses, `{"model":"m","input":"hi","store":true}`},
		{codec.ProtocolChatCompletions, `{"model":"m","messages":[{"role":"user","content":"hi"}],"store":true}`},
	}
	for _, c := range cases {
		ic, ok := codec.Inbound(c.proto)
		if !ok {
			t.Fatalf("inbound %q not registered", c.proto)
		}
		r, err := ic.DecodeRequest([]byte(c.body))
		if err != nil {
			t.Fatalf("%s DecodeRequest err=%v", c.proto, err)
		}
		if r.Store == nil || !*r.Store {
			t.Errorf("%s 显式 true 没进 IR：Store = %v", c.proto, r.Store)
		}
	}
	// 显式 false 保留表态、缺席不发明（以 responses 为代表，chat 同款判据）。
	ic, _ := codec.Inbound(codec.ProtocolResponses)
	r, err := ic.DecodeRequest([]byte(`{"model":"m","input":"hi","store":false}`))
	if err != nil {
		t.Fatalf("DecodeRequest err=%v", err)
	}
	if r.Store == nil || *r.Store {
		t.Errorf("显式 false 应保留表态：Store = %v", r.Store)
	}
	r, err = ic.DecodeRequest([]byte(`{"model":"m","input":"hi"}`))
	if err != nil {
		t.Fatalf("DecodeRequest err=%v", err)
	}
	if r.Store != nil {
		t.Errorf("缺席被发明成 %v", *r.Store)
	}
}

// 出站永不把客户端的 true 透传给上游：responses 强制写 store:false，其余三族
// 不写键。任何一族都不许出现 "store":true。
func TestStoreNeverPropagatedAsTrue(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 16, Store: boolPtr(true),
		Messages: []ir.Message{{Role: ir.RoleUser,
			Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
	for _, name := range codec.OutboundNames() {
		oc, _ := codec.Outbound(name)
		body, err := oc.EncodeRequest(req)
		if err != nil {
			t.Fatalf("%s: EncodeRequest err=%v", name, err)
		}
		if strings.Contains(string(body), `"store":true`) {
			t.Errorf("%s: 出站把客户端的 store:true 透传给了上游：%s", name, body)
		}
	}
}

// 诊断：显式 true 四族全报，显式 false 与缺席全静默——误报会让客户端对正常
// 请求起疑。store 不可兑现与目标无关（没有任何一族经本服务留存上游状态），
// 故四族措辞一致，不分「有槽位/无槽位」。
func TestDiagnoseStoreDropped(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 16, Store: boolPtr(true)}
	for _, name := range codec.OutboundNames() {
		oc, _ := codec.Outbound(name)
		got := strings.Join(codec.DescribeLossy(req, name, oc.Caps()), "; ")
		if !strings.Contains(got, "store") {
			t.Errorf("%s 丢弃 store 未报告：%q", name, got)
		}
		if !strings.Contains(got, "not be stored server-side") {
			t.Errorf("%s store 措辞应说明上游不留存：%q", name, got)
		}
	}
	for _, st := range []*bool{nil, boolPtr(false)} {
		bare := &ir.Request{Model: "m", MaxTokens: 16, Store: st}
		for _, name := range codec.OutboundNames() {
			oc, _ := codec.Outbound(name)
			if notes := codec.DescribeLossy(bare, name, oc.Caps()); len(notes) != 0 {
				t.Errorf("Store=%v 时 %s 误报：%v", st, name, notes)
			}
		}
	}
}
