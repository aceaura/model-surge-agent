package codec_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 本轮的轴线：上一轮打通了请求侧调参，客户端现在能提出这些要求，但结果
// 拿不到、也不知道拿不到。最坏的形态不是功能缺失而是「看起来生效了」——
// 客户端发 n:3 给一个支持它的目标，上游真回三路，我们静默扔两路，客户端
// 拿到一个 HTTP 200 的单候选回答，外观上与「目标不支持、报了有损」一致。

// multiCandidateStream 是各出站协议「上游回了三路候选」的流式夹具。
var multiCandidateStream = map[string]string{
	codec.ProtocolChatCompletions: "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[" +
		"{\"index\":0,\"delta\":{\"content\":\"one\"}}," +
		"{\"index\":1,\"delta\":{\"content\":\"two\"}}," +
		"{\"index\":2,\"delta\":{\"content\":\"three\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n",
	codec.ProtocolGemini: "data: {\"responseId\":\"g1\",\"modelVersion\":\"m\",\"candidates\":[" +
		"{\"index\":0,\"content\":{\"parts\":[{\"text\":\"one\"}]},\"finishReason\":\"STOP\"}," +
		"{\"index\":1,\"content\":{\"parts\":[{\"text\":\"two\"}]}}," +
		"{\"index\":2,\"content\":{\"parts\":[{\"text\":\"three\"}]}}]}\n\n",
}

// singleCandidateStream 是对照：只回一路。绝大多数请求是这个形态，
// 说明在这里出现就等于给每个正常请求都挂上一条噪音。
var singleCandidateStream = map[string]string{
	codec.ProtocolChatCompletions: "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":" +
		"[{\"index\":0,\"delta\":{\"content\":\"one\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n",
	codec.ProtocolGemini: "data: {\"responseId\":\"g1\",\"modelVersion\":\"m\",\"candidates\":" +
		"[{\"index\":0,\"content\":{\"parts\":[{\"text\":\"one\"}]},\"finishReason\":\"STOP\"}]}\n\n",
}

var multiCandidateWhole = map[string]string{
	codec.ProtocolChatCompletions: `{"id":"c1","model":"m","choices":[
		{"index":0,"message":{"role":"assistant","content":"one"},"finish_reason":"stop"},
		{"index":1,"message":{"role":"assistant","content":"two"},"finish_reason":"stop"},
		{"index":2,"message":{"role":"assistant","content":"three"},"finish_reason":"stop"}]}`,
	codec.ProtocolGemini: `{"responseId":"g1","modelVersion":"m","candidates":[
		{"index":0,"content":{"parts":[{"text":"one"}]},"finishReason":"STOP"},
		{"index":1,"content":{"parts":[{"text":"two"}]},"finishReason":"STOP"},
		{"index":2,"content":{"parts":[{"text":"three"}]},"finishReason":"STOP"}]}`,
}

var singleCandidateWhole = map[string]string{
	codec.ProtocolChatCompletions: `{"id":"c1","model":"m","choices":[
		{"index":0,"message":{"role":"assistant","content":"one"},"finish_reason":"stop"}]}`,
	codec.ProtocolGemini: `{"responseId":"g1","modelVersion":"m","candidates":[
		{"index":0,"content":{"parts":[{"text":"one"}]},"finishReason":"STOP"}]}`,
}

// decodeStreamNotes 喂完一段流后取解码器的说明。
// 不复用 decodeStream：那个助手不返回解码器，拿不到 Notes。
func decodeStreamNotes(t *testing.T, protocol, raw string) []string {
	t.Helper()
	c, ok := codec.Outbound(protocol)
	if !ok {
		t.Fatalf("outbound %q not registered", protocol)
	}
	dec := c.NewStreamDecoder()
	scanner := codec.NewFrameScanner(strings.NewReader(raw))
	for scanner.Scan() {
		frame := scanner.Frame()
		if _, err := dec.Feed(frame.Event, frame.Data); err != nil {
			t.Fatalf("%s feed %q: %v", protocol, frame.Data, err)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("%s scan: %v", protocol, err)
	}
	dec.Finish()
	n, ok := dec.(codec.StreamNotes)
	if !ok {
		t.Fatalf("%s 的解码器必须实现 StreamNotes", protocol)
	}
	return n.Notes()
}

// decodeWholeNotes 走非流式的有损解码出口。
func decodeWholeNotes(t *testing.T, protocol, body string) (*ir.Response, []string) {
	t.Helper()
	c, ok := codec.Outbound(protocol)
	if !ok {
		t.Fatalf("outbound %q not registered", protocol)
	}
	d, ok := c.(codec.LossyResponseDecoder)
	if !ok {
		t.Fatalf("%s 有候选数组，必须实现 LossyResponseDecoder", protocol)
	}
	resp, notes, err := d.DecodeResponseLossy([]byte(body))
	if err != nil {
		t.Fatalf("%s DecodeResponseLossy: %v", protocol, err)
	}
	// 顺带钉住两条路径等价：普通解码不该因为少了说明而解出别的内容。
	plain, err := c.DecodeResponse([]byte(body))
	if err != nil {
		t.Fatalf("%s DecodeResponse: %v", protocol, err)
	}
	if !sameResponse(resp, plain) {
		t.Fatalf("%s: 两条解码路径产出不同响应\n lossy: %+v\n plain: %+v", protocol, resp, plain)
	}
	return resp, notes
}

func sameResponse(a, b *ir.Response) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func hasNote(notes []string, substr string) bool {
	for _, n := range notes {
		if strings.Contains(n, substr) {
			return true
		}
	}
	return false
}

func countNotes(notes []string, substr string) int {
	n := 0
	for _, s := range notes {
		if strings.Contains(s, substr) {
			n++
		}
	}
	return n
}

// TestMultiCandidateStreamIsReported：流式多候选必须报，且带实际路数。
func TestMultiCandidateStreamIsReported(t *testing.T) {
	for proto, raw := range multiCandidateStream {
		t.Run(proto, func(t *testing.T) {
			notes := decodeStreamNotes(t, proto, raw)
			if !hasNote(notes, "extra response candidate") {
				t.Fatalf("多候选必须报说明: %v", notes)
			}
			// 数字必须是「丢了几路」而不是「上游回了几路」：
			// 三路里留一路，丢的是两路。
			if !hasNote(notes, "dropped 2 extra") {
				t.Errorf("路数不对，应为 2: %v", notes)
			}
			// 一个流恒一条。逐帧生成说明时 index 1 与 index 2 会各报一条，
			// 数字不同、字符串去重挡不住。
			if got := countNotes(notes, "extra response candidate"); got != 1 {
				t.Errorf("一个流只该报一条多候选说明，得到 %d 条: %v", got, notes)
			}
		})
	}
}

// TestSingleCandidateIsNotReported 是对照：只回一路时一条都不报。
func TestSingleCandidateIsNotReported(t *testing.T) {
	for proto, raw := range singleCandidateStream {
		t.Run(proto+"/stream", func(t *testing.T) {
			if notes := decodeStreamNotes(t, proto, raw); hasNote(notes, "extra response candidate") {
				t.Errorf("只回一路不该报多候选说明: %v", notes)
			}
		})
	}
	for proto, body := range singleCandidateWhole {
		t.Run(proto+"/whole", func(t *testing.T) {
			if _, notes := decodeWholeNotes(t, proto, body); hasNote(notes, "extra response candidate") {
				t.Errorf("只回一路不该报多候选说明: %v", notes)
			}
		})
	}
}

// TestMultiCandidateWholeIsReported：非流式路径同样要报。
// 这条单列是因为它走的是另一个出口（LossyResponseDecoder 而非 StreamNotes），
// 只测流式时非流式那条可以整段删掉而测试仍绿。
func TestMultiCandidateWholeIsReported(t *testing.T) {
	for proto, body := range multiCandidateWhole {
		t.Run(proto, func(t *testing.T) {
			resp, notes := decodeWholeNotes(t, proto, body)
			if !hasNote(notes, "dropped 2 extra") {
				t.Fatalf("非流式多候选必须报说明且路数为 2: %v", notes)
			}
			// 内容仍然只取第一路——报说明不改变塌缩这个既有行为。
			body, _ := json.Marshal(resp)
			if strings.Contains(string(body), "two") || strings.Contains(string(body), "three") {
				t.Errorf("中立表示只该装第一路: %s", body)
			}
		})
	}
}

// serviceTierStream 是「上游把档位降到 default」的夹具：
// 客户端点的是 flex，上游回的是 default。
var serviceTierStream = map[string]string{
	codec.ProtocolChatCompletions: "data: {\"id\":\"c1\",\"model\":\"m\",\"service_tier\":\"default\"," +
		"\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n",
	codec.ProtocolResponses: "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"model\":\"m\",\"service_tier\":\"default\"}}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"model\":\"m\",\"status\":\"completed\",\"service_tier\":\"default\"}}\n\n",
}

var serviceTierWhole = map[string]string{
	codec.ProtocolChatCompletions: `{"id":"c1","model":"m","service_tier":"default","choices":[
		{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`,
	codec.ProtocolResponses: `{"id":"r1","model":"m","status":"completed","service_tier":"default",
		"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}`,
}

// TestServiceTierDecodesIntoIR：两个有这个字段的出站协议都要解出它。
func TestServiceTierDecodesIntoIR(t *testing.T) {
	for proto, raw := range serviceTierStream {
		t.Run(proto+"/stream", func(t *testing.T) {
			var got string
			for _, ev := range decodeStream(t, proto, raw) {
				if ev.ServiceTier != "" {
					got = ev.ServiceTier
				}
			}
			if got != "default" {
				t.Fatalf("service_tier 没解进事件，得到 %q", got)
			}
		})
	}
	for proto, body := range serviceTierWhole {
		t.Run(proto+"/whole", func(t *testing.T) {
			c, _ := codec.Outbound(proto)
			resp, err := c.DecodeResponse([]byte(body))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.ServiceTier != "default" {
				t.Fatalf("service_tier 没解进响应，得到 %q", resp.ServiceTier)
			}
		})
	}
}

// serviceTierLateStream 是「档位只在后续帧/收尾帧出现」的夹具。
//
// 与 serviceTierStream 分开是必需的：那份夹具的每一帧都带档位，于是
// 「首帧直接写进 message_start」与「跨帧累积」两条捕获路径同时生效，
// 删掉任何一条测试仍绿。真实上游两种形态都有——有的每个 chunk 都带，
// 有的只在末尾那帧带。
var serviceTierLateStream = map[string]string{
	// 首帧不带，第二帧才带。
	codec.ProtocolChatCompletions: "data: {\"id\":\"c1\",\"model\":\"m\"," +
		"\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"service_tier\":\"default\"," +
		"\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n",
	// completed 帧不带，只有 created 带。
	codec.ProtocolResponses: "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"model\":\"m\",\"service_tier\":\"default\"}}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"model\":\"m\",\"status\":\"completed\"}}\n\n",
}

// TestServiceTierFromASingleFrameSurvives：档位只在一处出现时也不能丢。
func TestServiceTierFromASingleFrameSurvives(t *testing.T) {
	for proto, raw := range serviceTierLateStream {
		t.Run(proto, func(t *testing.T) {
			var agg ir.Aggregator
			for _, ev := range decodeStream(t, proto, raw) {
				agg.Add(ev)
			}
			if got := agg.Response().ServiceTier; got != "default" {
				t.Fatalf("档位只在一处出现时丢了，得到 %q", got)
			}
		})
	}
}

// TestServiceTierIsEchoedToClient：两个有位置的入站协议都要写出它。
func TestServiceTierIsEchoedToClient(t *testing.T) {
	events := []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "m1", Model: "m", ServiceTier: "default"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 0, Text: "hi"},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, ServiceTier: "default"},
		{Type: ir.EvMessageStop},
	}
	for _, proto := range []string{codec.ProtocolChatCompletions, codec.ProtocolResponses} {
		t.Run(proto+"/stream", func(t *testing.T) {
			if out := renderStream(t, proto, events); !strings.Contains(out, `"service_tier":"default"`) {
				t.Fatalf("流式没回显 service_tier: %s", out)
			}
		})
		t.Run(proto+"/whole", func(t *testing.T) {
			c, _ := codec.Inbound(proto)
			body, err := c.EncodeResponse(&ir.Response{
				ID: "m1", Model: "m", ServiceTier: "default",
				Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}},
			})
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if !strings.Contains(string(body), `"service_tier":"default"`) {
				t.Fatalf("非流式没回显 service_tier: %s", body)
			}
		})
	}
}

// TestServiceTierDowngradeIsVisible 是本轮 service_tier 那半边的核心断言：
// 客户端点 flex、上游降到 default，客户端必须看到 default。
//
// 单列而不并进上一条：拿请求里的值兜底这个变异在「上游回的与请求相同」的
// 夹具上完全测不出来，而那正是最容易写出的兜底代码。
func TestServiceTierDowngradeIsVisible(t *testing.T) {
	for proto, raw := range serviceTierStream {
		t.Run(proto, func(t *testing.T) {
			events := decodeStream(t, proto, raw)
			// 客户端请求的是 flex；IR 里的响应侧档位必须是上游说的那个。
			var agg ir.Aggregator
			for _, ev := range events {
				agg.Add(ev)
			}
			resp := agg.Response()
			if resp.ServiceTier == "flex" {
				t.Fatalf("回显了请求里的档位而不是上游的：降档被伪装成了按要求执行")
			}
			if resp.ServiceTier != "default" {
				t.Fatalf("service_tier 应为上游回的 default，得到 %q", resp.ServiceTier)
			}
		})
	}
}

// TestAbsentServiceTierIsNotWritten：上游没回就一个字都不写。
// 写出空串会让客户端以为上游明确说了「空档位」。
func TestAbsentServiceTierIsNotWritten(t *testing.T) {
	events := []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "m1", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 0, Text: "hi"},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn},
		{Type: ir.EvMessageStop},
	}
	for _, proto := range inboundNames() {
		t.Run(proto+"/stream", func(t *testing.T) {
			if out := renderStream(t, proto, events); strings.Contains(out, "service_tier") {
				t.Errorf("上游没回档位，响应里不该出现该键: %s", out)
			}
		})
		t.Run(proto+"/whole", func(t *testing.T) {
			c, _ := codec.Inbound(proto)
			body, err := c.EncodeResponse(&ir.Response{
				ID: "m1", Model: "m", Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}},
			})
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if strings.Contains(string(body), "service_tier") {
				t.Errorf("上游没回档位，响应里不该出现该键: %s", body)
			}
		})
	}
}

// TestAnthropicReportsDroppedServiceTier：无处安放时必须说一声。
// 这一维决定计费，无声丢掉会让客户端按自己点的档位对账。
func TestAnthropicReportsDroppedServiceTier(t *testing.T) {
	c, ok := codec.Inbound(codec.ProtocolAnthropic)
	if !ok {
		t.Fatal("anthropic inbound 未注册")
	}

	t.Run("whole", func(t *testing.T) {
		le, ok := c.(codec.LossyResponseEncoder)
		if !ok {
			t.Fatal("anthropic 必须实现 LossyResponseEncoder")
		}
		_, notes, err := le.EncodeResponseLossy(&ir.Response{
			ID: "m1", Model: "m", ServiceTier: "default",
			Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}},
		})
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if !hasNote(notes, "service_tier") {
			t.Fatalf("必须报丢弃 service_tier: %v", notes)
		}
	})

	t.Run("stream", func(t *testing.T) {
		enc := c.NewStreamEncoder()
		for _, ev := range []ir.Event{
			{Type: ir.EvMessageStart, MessageID: "m1", Model: "m", ServiceTier: "default"},
			{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn},
			{Type: ir.EvMessageStop},
		} {
			if _, err := enc.Encode(ev); err != nil {
				t.Fatalf("encode %s: %v", ev.Type, err)
			}
		}
		enc.Finish()
		n, ok := enc.(codec.StreamNotes)
		if !ok {
			t.Fatal("anthropic 编码器必须实现 StreamNotes")
		}
		if !hasNote(n.Notes(), "service_tier") {
			t.Fatalf("流式也必须报丢弃 service_tier: %v", n.Notes())
		}
	})

	// 收尾帧才带档位的形态（非流式响应投影成事件时就是这样）：
	// 只在 message_start 判会漏掉它。
	t.Run("stream/only-on-delta", func(t *testing.T) {
		enc := c.NewStreamEncoder()
		for _, ev := range []ir.Event{
			{Type: ir.EvMessageStart, MessageID: "m1", Model: "m"},
			{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, ServiceTier: "default"},
			{Type: ir.EvMessageStop},
		} {
			if _, err := enc.Encode(ev); err != nil {
				t.Fatalf("encode %s: %v", ev.Type, err)
			}
		}
		enc.Finish()
		n := enc.(codec.StreamNotes)
		if !hasNote(n.Notes(), "service_tier") {
			t.Fatalf("档位只在收尾帧到达时也必须报: %v", n.Notes())
		}
	})
}

// TestAggregatorMergesServiceTier：两帧先后顺序与非空覆盖。
//
// 「先到粘住」与「非空覆盖」在只有一帧带值时行为相同，必须用两帧才能分开。
func TestAggregatorMergesServiceTier(t *testing.T) {
	cases := []struct {
		name   string
		events []ir.Event
		want   string
	}{
		{"只有 start 带", []ir.Event{
			{Type: ir.EvMessageStart, ServiceTier: "flex"},
			{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn},
		}, "flex"},
		{"只有 delta 带", []ir.Event{
			{Type: ir.EvMessageStart},
			{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, ServiceTier: "flex"},
		}, "flex"},
		{"两帧都带，后到者胜", []ir.Event{
			{Type: ir.EvMessageStart, ServiceTier: "flex"},
			{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, ServiceTier: "default"},
		}, "default"},
		{"后到为空不清零", []ir.Event{
			{Type: ir.EvMessageStart, ServiceTier: "default"},
			{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn},
		}, "default"},
		{"一帧都没带", []ir.Event{
			{Type: ir.EvMessageStart},
			{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn},
		}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var agg ir.Aggregator
			for _, ev := range c.events {
				agg.Add(ev)
			}
			if got := agg.Response().ServiceTier; got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestResponseEventsCarriesServiceTier：整份响应投影成事件时不能把它落下。
// 上游忽略流式请求、回一整份响应时走的就是这条路。
func TestResponseEventsCarriesServiceTier(t *testing.T) {
	events := ir.ResponseEvents(&ir.Response{
		ID: "m1", Model: "m", ServiceTier: "default",
		Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}},
	})
	var agg ir.Aggregator
	for _, ev := range events {
		agg.Add(ev)
	}
	if got := agg.Response().ServiceTier; got != "default" {
		t.Fatalf("投影丢了 service_tier，得到 %q", got)
	}
}

// TestFinishMessageIsReported：上游附的人类可读收尾原因不能无声消失。
func TestFinishMessageIsReported(t *testing.T) {
	const detail = "blocked by policy XYZ"
	raw := "data: {\"responseId\":\"g1\",\"modelVersion\":\"m\",\"candidates\":[{\"index\":0," +
		"\"content\":{\"parts\":[{\"text\":\"hi\"}]},\"finishReason\":\"SAFETY\"," +
		"\"finishMessage\":\"" + detail + "\"}]}\n\n"

	t.Run("stream", func(t *testing.T) {
		notes := decodeStreamNotes(t, codec.ProtocolGemini, raw)
		if !hasNote(notes, detail) {
			t.Fatalf("收尾原因原文必须带上: %v", notes)
		}
	})

	t.Run("whole", func(t *testing.T) {
		body := `{"responseId":"g1","modelVersion":"m","candidates":[{"index":0,
			"content":{"parts":[{"text":"hi"}]},"finishReason":"SAFETY",
			"finishMessage":"` + detail + `"}]}`
		_, notes := decodeWholeNotes(t, codec.ProtocolGemini, body)
		if !hasNote(notes, detail) {
			t.Fatalf("非流式也必须带原文: %v", notes)
		}
	})

	// 原文由上游决定长度，说明要落库，必须截断。
	t.Run("截断", func(t *testing.T) {
		long := strings.Repeat("x", 500)
		note := codec.FinishDetailNote(long)
		if len(note) > 300 {
			t.Fatalf("说明未截断，长度 %d", len(note))
		}
		if !strings.HasSuffix(note, "...") {
			t.Errorf("截断后应有省略标记: %s", note)
		}
	})

	// finishReason 已经决定了枚举值，finishMessage 只是补充描述，
	// 拿它改枚举会让两个来源打架。
	//
	// 这里刻意用 STOP 而不是上面那份 SAFETY 夹具：SAFETY 本就映射成
	// StopContentFilter，拿 finishMessage 覆写成同一个值是行为等价的，
	// 测不出「谁说了算」。只有原枚举与覆写值不同才暴露打架。
	t.Run("不改 StopReason", func(t *testing.T) {
		normal := "data: {\"responseId\":\"g1\",\"modelVersion\":\"m\",\"candidates\":[{\"index\":0," +
			"\"content\":{\"parts\":[{\"text\":\"hi\"}]},\"finishReason\":\"STOP\"," +
			"\"finishMessage\":\"" + detail + "\"}]}\n\n"
		var agg ir.Aggregator
		for _, ev := range decodeStream(t, codec.ProtocolGemini, normal) {
			agg.Add(ev)
		}
		if got := agg.Response().StopReason; got != ir.StopEndTurn {
			t.Fatalf("StopReason 应仍由 finishReason 决定，得到 %q", got)
		}
	})
}

// TestLogProbsNoteDistinguishesTwoCases 是本轮请求侧唯一的改动：
// 「目标表达不了」与「发出去了但结果不回传」是两件事，措辞必须分开——
// 前者换个目标就有，后者换谁都没有。
func TestLogProbsNoteDistinguishesTwoCases(t *testing.T) {
	req := paramBase()
	on := true
	req.LogProbs = &on

	for _, out := range outboundNames() {
		t.Run(out, func(t *testing.T) {
			_, notes := lossyOf(t, out, req)
			supported := supportsField(t, out, "logprobs")
			dropped := containsDroppedField(notes, "logprobs")
			unreturned := hasNote(notes, "forwarded logprobs")
			switch {
			case supported && !unreturned:
				t.Errorf("%s 支持 logprobs，必须报「发出去但不回传」: %v", out, notes)
			case supported && dropped:
				t.Errorf("%s 支持 logprobs，不该报丢弃: %v", out, notes)
			case !supported && !dropped:
				t.Errorf("%s 表达不了 logprobs，必须报丢弃: %v", out, notes)
			case !supported && unreturned:
				t.Errorf("%s 表达不了 logprobs，不该报「发出去」: %v", out, notes)
			}
		})
	}
}

// TestSupportedCandidatesIsNotReportedOnRequest 是两格互斥的另一半：
// 目标支持 n 时请求侧不报，实际丢了几路由响应侧报——那个数字更有用。
func TestSupportedCandidatesIsNotReportedOnRequest(t *testing.T) {
	req := paramBase()
	three := 3
	req.Candidates = &three
	for _, out := range outboundNames() {
		if !supportsField(t, out, "n") {
			continue
		}
		t.Run(out, func(t *testing.T) {
			_, notes := lossyOf(t, out, req)
			if containsDroppedField(notes, "n") {
				t.Fatalf("%s 支持 n，请求侧不该报丢弃: %v", out, notes)
			}
		})
	}
}

// TestEveryOutboundWithCandidatesImplementsLossyDecoder 是守卫：
// 新增一个响应里带候选数组的出站协议时，必须一并给出非流式的说明出口，
// 否则那条路径上的塌缩会重新变成无声的。
func TestEveryOutboundWithCandidatesImplementsLossyDecoder(t *testing.T) {
	for _, name := range outboundNames() {
		c, _ := codec.Outbound(name)
		if !c.Caps().Candidates {
			continue
		}
		if _, ok := c.(codec.LossyResponseDecoder); !ok {
			t.Errorf("%s 支持多候选，必须实现 LossyResponseDecoder", name)
		}
	}
}
