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

// responses 的 background 字段此前在 DTO 里没有声明，客户端给的值静默蒸发：
// 它以为提交了一个异步任务，实际得到的是同步阻塞的完整回答，且没有任何
// 痕迹可查。收进 IR 后：出站一律不回写（本服务对上游恒 stream:true +
// store:false，与官方 background 的前置条件相反，写回去是保证 400 的矛盾
// 请求），显式 true 一律由 DescribeLossy 报出——responses 有槽位但兑现
// 不了，措辞与无槽位三族区分。

func boolPtr(b bool) *bool { return &b }

// 解码可见性：三态各自归位，缺席不发明。
func TestBackgroundDecodeIntoIR(t *testing.T) {
	ic, ok := codec.Inbound(codec.ProtocolResponses)
	if !ok {
		t.Fatalf("inbound %q not registered", codec.ProtocolResponses)
	}
	r, err := ic.DecodeRequest([]byte(`{"model":"m","input":"hi","background":true}`))
	if err != nil {
		t.Fatalf("DecodeRequest err=%v", err)
	}
	if r.Background == nil || !*r.Background {
		t.Errorf("显式 true 没进 IR：Background = %v", r.Background)
	}
	r, err = ic.DecodeRequest([]byte(`{"model":"m","input":"hi","background":false}`))
	if err != nil {
		t.Fatalf("DecodeRequest err=%v", err)
	}
	if r.Background == nil || *r.Background {
		t.Errorf("显式 false 应保留表态：Background = %v", r.Background)
	}
	r, err = ic.DecodeRequest([]byte(`{"model":"m","input":"hi"}`))
	if err != nil {
		t.Fatalf("DecodeRequest err=%v", err)
	}
	if r.Background != nil {
		t.Errorf("缺席被发明成 %v", *r.Background)
	}
}

// 出站不回写：四族的载荷里都不许出现 background 键——responses 也不许，
// 与强制的 stream:true + store:false 同发是矛盾请求。
func TestBackgroundNeverWrittenOutbound(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 16, Background: boolPtr(true),
		Messages: []ir.Message{{Role: ir.RoleUser,
			Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
	for _, name := range codec.OutboundNames() {
		oc, _ := codec.Outbound(name)
		body, err := oc.EncodeRequest(req)
		if err != nil {
			t.Fatalf("%s: EncodeRequest err=%v", name, err)
		}
		if strings.Contains(string(body), `"background"`) {
			t.Errorf("%s: 出站发明了 background 键：%s", name, body)
		}
	}
}

// 诊断：显式 true 四族全报（responses 走「有槽位但兑现不了」的措辞），
// 显式 false 与缺席全静默——误报会让客户端对正常请求起疑。
func TestDiagnoseBackgroundDropped(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 16, Background: boolPtr(true)}
	for _, name := range codec.OutboundNames() {
		oc, _ := codec.Outbound(name)
		got := strings.Join(codec.DescribeLossy(req, name, oc.Caps()), "; ")
		if !strings.Contains(got, "background") {
			t.Errorf("%s 丢弃 background 未报告：%q", name, got)
		}
		if name == codec.ProtocolResponses {
			if !strings.Contains(got, "cannot be honored") {
				t.Errorf("responses 应走「兑现不了」措辞：%q", got)
			}
		} else if !strings.Contains(got, "no background mode") {
			t.Errorf("%s 应走「无槽位」措辞：%q", name, got)
		}
	}
	for _, bg := range []*bool{nil, boolPtr(false)} {
		bare := &ir.Request{Model: "m", MaxTokens: 16, Background: bg}
		for _, name := range codec.OutboundNames() {
			oc, _ := codec.Outbound(name)
			if notes := codec.DescribeLossy(bare, name, oc.Caps()); len(notes) != 0 {
				t.Errorf("Background=%v 时 %s 误报：%v", bg, name, notes)
			}
		}
	}
}
