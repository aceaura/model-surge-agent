package codec_test

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	_ "github.com/aceaura/model-surge-agent/backend/codec/anthropic"
	_ "github.com/aceaura/model-surge-agent/backend/codec/chatcompletions"
	_ "github.com/aceaura/model-surge-agent/backend/codec/responses"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 档位回显（响应侧）值集映射：回显语义是「实际用了哪档容量」，值集与
// 请求侧偏好不同——anthropic 回显 standard/priority/batch，chat 回显
// auto/default/flex/scale/priority/fast，responses 另有 ultrafast。
// 越集丢弃必须带值报出（档位是官方枚举非敏感）；谁都不认识的值原样
// 透传（与请求侧 MapServiceTier 同一口径：可能是官方新增档位，丢掉
// 比让客户端见到陌生枚举更糟——计费对不上账）。
//
// responseLossyNotes / streamNotes 复用同包既有助手。
//
// 对应旧仓 #29（9164308）。

func TestMapServiceTierEcho(t *testing.T) {
	cases := []struct {
		tier, proto, want string
		ok                bool
	}{
		// 方言归一：standard（anthropic 回显）/ standard_only（anthropic
		// 请求）与 default（OpenAI 回显）同为标准容量，互译。
		{"standard", codec.ProtocolAnthropic, "standard", true},
		{"standard_only", codec.ProtocolAnthropic, "standard", true},
		{"default", codec.ProtocolAnthropic, "standard", true},
		{"standard", codec.ProtocolChatCompletions, "default", true},
		{"standard", codec.ProtocolResponses, "default", true},
		// priority 三家都回显得出，恒通。
		{"priority", codec.ProtocolAnthropic, "priority", true},
		{"priority", codec.ProtocolChatCompletions, "priority", true},
		{"priority", codec.ProtocolResponses, "priority", true},
		// batch 只有 anthropic 回显得出。
		{"batch", codec.ProtocolAnthropic, "batch", true},
		{"batch", codec.ProtocolChatCompletions, "", false},
		{"batch", codec.ProtocolResponses, "", false},
		// ultrafast 仅 responses 系。
		{"ultrafast", codec.ProtocolResponses, "ultrafast", true},
		{"ultrafast", codec.ProtocolChatCompletions, "", false},
		{"ultrafast", codec.ProtocolAnthropic, "", false},
		// OpenAI 系回显去 anthropic 无等价。
		{"auto", codec.ProtocolAnthropic, "", false},
		{"flex", codec.ProtocolAnthropic, "", false},
		{"scale", codec.ProtocolAnthropic, "", false},
		{"fast", codec.ProtocolAnthropic, "", false},
		// OpenAI 两系本族值恒通。
		{"auto", codec.ProtocolChatCompletions, "auto", true},
		{"flex", codec.ProtocolChatCompletions, "flex", true},
		{"scale", codec.ProtocolChatCompletions, "scale", true},
		{"fast", codec.ProtocolChatCompletions, "fast", true},
		{"default", codec.ProtocolResponses, "default", true},
		// 谁都不认识的值：可能是官方新增档位，透传不丢。
		{"brand-new-tier", codec.ProtocolAnthropic, "brand-new-tier", true},
		{"brand-new-tier", codec.ProtocolChatCompletions, "brand-new-tier", true},
		{"brand-new-tier", codec.ProtocolResponses, "brand-new-tier", true},
		// 没有回显槽位的协议（gemini 在本服务只出站）恒 false。
		{"default", codec.ProtocolGemini, "", false},
		{"priority", codec.ProtocolGemini, "", false},
		// 空值恒通（omitempty 下等于没写）。
		{"", codec.ProtocolAnthropic, "", true},
	}
	for _, c := range cases {
		got, ok := codec.MapServiceTierEcho(c.tier, c.proto)
		if got != c.want || ok != c.ok {
			t.Errorf("MapServiceTierEcho(%q, %s) = (%q, %v)，想要 (%q, %v)",
				c.tier, c.proto, got, ok, c.want, c.ok)
		}
	}
}

// tierInbound 有档位回显槽位的三个入站协议。
var tierInbound = []string{codec.ProtocolAnthropic, codec.ProtocolChatCompletions, codec.ProtocolResponses}

func TestTierEchoNotesPerTarget(t *testing.T) {
	// 越集回显：batch 只有 anthropic 装得下；ultrafast 只有 responses 装得下。
	for _, c := range []struct {
		tier    string
		silent  string
		reports []string
	}{
		{"batch", codec.ProtocolAnthropic,
			[]string{codec.ProtocolChatCompletions, codec.ProtocolResponses}},
		{"ultrafast", codec.ProtocolResponses,
			[]string{codec.ProtocolAnthropic, codec.ProtocolChatCompletions}},
	} {
		resp := &ir.Response{ID: "m", Model: "m", ServiceTier: c.tier}
		if notes := responseLossyNotes(t, c.silent, resp); hasNote(notes, "service tier echo") {
			t.Errorf("%s 装得下 %q，误报：%v", c.silent, c.tier, notes)
		}
		for _, name := range c.reports {
			notes := responseLossyNotes(t, name, resp)
			if !hasNote(notes, `service tier echo "`+c.tier+`"`) {
				t.Errorf("%s 装不下 %q，应带值报出：%v", name, c.tier, notes)
			}
		}
	}
	// flex 去 anthropic 无等价；chat/responses 装得下。
	resp := &ir.Response{ID: "m", Model: "m", ServiceTier: "flex"}
	if notes := responseLossyNotes(t, codec.ProtocolAnthropic, resp); !hasNote(notes, `service tier echo "flex"`) {
		t.Errorf("anthropic 装不下 flex，应带值报出：%v", notes)
	}
	for _, name := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses} {
		if notes := responseLossyNotes(t, name, resp); hasNote(notes, "service tier echo") {
			t.Errorf("%s 装得下 flex，误报：%v", name, notes)
		}
	}
	// 可互译与缺席全静默。
	for _, tier := range []string{"", "default", "priority"} {
		r := &ir.Response{ID: "m", Model: "m", ServiceTier: tier}
		for _, name := range tierInbound {
			if notes := responseLossyNotes(t, name, r); hasNote(notes, "service tier echo") {
				t.Errorf("%s->%s 档位 %q 不该报：%v", tier, name, tier, notes)
			}
		}
	}
}

func TestTierEchoStreamNotes(t *testing.T) {
	// 越集档位在流式通道同样报出，且不写进任何帧。
	for _, c := range []struct{ tier, target string }{
		{"batch", codec.ProtocolChatCompletions},
		{"batch", codec.ProtocolResponses},
		{"ultrafast", codec.ProtocolChatCompletions},
		{"flex", codec.ProtocolAnthropic},
	} {
		ic, _ := codec.Inbound(c.target)
		e := ic.NewStreamEncoder(nil)
		frames, err := e.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "m", Model: "m", ServiceTier: c.tier})
		if err != nil {
			t.Fatalf("%s encode start: %v", c.target, err)
		}
		e.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn})
		e.Encode(ir.Event{Type: ir.EvMessageStop})
		for _, f := range frames {
			if strings.Contains(string(f), "service_tier") {
				t.Errorf("%s->%s 越集档位不该写进帧：%s", c.tier, c.target, f)
			}
		}
		if got := streamNotes(e); !strings.Contains(got, `service tier echo "`+c.tier+`"`) {
			t.Errorf("%s->%s 应带值报出：%q", c.tier, c.target, got)
		}
	}
	// 装得下的档位静默下发。anthropic 首帧带翻译后的 standard；chat 的
	// 晚到回显（EvMessageDelta 才带）也补得上。
	ic, _ := codec.Inbound(codec.ProtocolAnthropic)
	e := ic.NewStreamEncoder(nil)
	frames, _ := e.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "m", Model: "m", ServiceTier: "default"})
	if !strings.Contains(string(frames[0]), `"service_tier":"standard"`) {
		t.Errorf("anthropic 首帧应回显 standard：%s", frames[0])
	}
	e.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn})
	if got := streamNotes(e); got != "" {
		t.Errorf("anthropic 装得下的回显误报：%q", got)
	}

	ic, _ = codec.Inbound(codec.ProtocolChatCompletions)
	e = ic.NewStreamEncoder(nil)
	e.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "m", Model: "m"})
	e.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, ServiceTier: "default"})
	tail, err := e.Encode(ir.Event{Type: ir.EvMessageStop})
	if err != nil {
		t.Fatalf("chat encode stop: %v", err)
	}
	var joined string
	for _, f := range tail {
		joined += string(f)
	}
	if !strings.Contains(joined, `"service_tier":"default"`) {
		t.Errorf("chat 晚到的回显应挂在收尾 chunk 上：%s", joined)
	}
	if got := streamNotes(e); strings.Contains(got, "service tier echo") {
		t.Errorf("chat 晚到回显补得上，不该报丢：%q", got)
	}
}
