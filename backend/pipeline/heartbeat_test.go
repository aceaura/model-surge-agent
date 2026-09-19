package pipeline_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aceaura/model-surge-agent/backend/capture"
	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/pipeline"
	"github.com/aceaura/model-surge-agent/backend/relaymock"
)

// silentAfterFirstFrame 发头两帧建流，然后静默一段时间，再发完余下的。
//
// 这就是要挡的那个形态：推理模型在首帧之后进入长思考，中间设施在 30~60s
// 无字节时掐连接，而上游一切正常。
func silentAfterFirstFrame(silence time.Duration) func(int, http.ResponseWriter) {
	return func(_ int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		head, tail, _ := strings.Cut(okStream, "event: content_block_delta")
		_, _ = w.Write([]byte(head))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(silence)
		_, _ = w.Write([]byte("event: content_block_delta" + tail))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// tuneOpts 改一份 fixture 的流水线参数。
//
// 直接改 f.p.Opts 而不是给 newFixture 加参数：本轮只有心跳用例要调它，
// 加参数会让既有的几十个调用点全部跟改。
func tuneOpts(f *fixture, hb, firstToken, idle time.Duration) {
	f.p.Opts = pipeline.Options{
		MaxAttempts:       3,
		FirstTokenTimeout: firstToken,
		IdleTimeout:       idle,
		HeartbeatInterval: hb,
	}
}

// 静默期内必须发出保活帧，且 anthropic 客户端收到的是 ping 事件。
func TestHeartbeatSentDuringSilence(t *testing.T) {
	up := &fakeUpstream{handler: silentAfterFirstFrame(300 * time.Millisecond)}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})
	tuneOpts(f, 50*time.Millisecond, 2*time.Second, 2*time.Second)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	out := w.Body.String()
	if !strings.Contains(out, "event: ping") {
		t.Errorf("静默期内没发保活帧，中间设施会掐掉这条连接：%q", out)
	}
	// 内容照常交付：保活不能顶替或打乱正常帧。
	if !strings.Contains(out, "hello") || !strings.Contains(out, "message_stop") {
		t.Errorf("保活挤掉了正常内容：%q", out)
	}
	rec := f.col.record(t)
	if rec.Outcome != "normal" {
		t.Errorf("outcome = %q", rec.Outcome)
	}
}

// 保活帧不得进记账：它不是内容。
//
// 与上一条用同一个静默装置，但断言的是 usage 与上游报的一致。
func TestHeartbeatDoesNotAffectAccounting(t *testing.T) {
	up := &fakeUpstream{handler: silentAfterFirstFrame(300 * time.Millisecond)}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})
	tuneOpts(f, 50*time.Millisecond, 2*time.Second, 2*time.Second)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if !strings.Contains(w.Body.String(), "event: ping") {
		t.Fatal("这条用例的前提是真发过保活帧，但一帧都没发")
	}
	rec := f.col.record(t)
	// okStream 里 input=100、output=7，保活若被当成事件会把 output 推高。
	if rec.Usage.InputTokens != 100 || rec.Usage.OutputTokens != 7 {
		t.Errorf("usage 被保活帧改了：in=%d out=%d，想要 100/7",
			rec.Usage.InputTokens, rec.Usage.OutputTokens)
	}
}

// 保活帧必须进捕获：客户端确实收到了这些字节，
// 排查「流里有奇怪帧」时需要它们在场。
func TestHeartbeatEntersCapture(t *testing.T) {
	up := &fakeUpstream{handler: silentAfterFirstFrame(300 * time.Millisecond)}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})
	tuneOpts(f, 50*time.Millisecond, 2*time.Second, 2*time.Second)

	st := capture.New(capture.Options{Mode: capture.ModeAll})
	c := call(t, true)
	c.Capture = st.Begin(c.RequestID)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, c)
	if !strings.Contains(w.Body.String(), "event: ping") {
		t.Fatal("这条用例的前提是真发过保活帧")
	}

	got := string(snapshotOf(t, st, c.RequestID).Bodies[capture.ClientResponse].Bytes)
	if !strings.Contains(got, "event: ping") {
		t.Errorf("保活帧没进捕获，客户端收到的字节与捕获里的不一致：%q", got)
	}
}

// 关键的一条：保活不得让空闲超时失效。
//
// 上游发完首帧之后彻底静默。心跳间隔远小于空闲超时，所以在空闲超时到点
// 之前会发出很多个保活帧——若计时器被每次唤醒重置，这条流会永远挂着。
func TestHeartbeatDoesNotPostponeIdleTimeout(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		head, _, _ := strings.Cut(okStream, "event: content_block_delta")
		_, _ = w.Write([]byte(head))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// 挂到测试结束，模拟上游静默黑洞。
		time.Sleep(10 * time.Second)
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})
	tuneOpts(f, 20*time.Millisecond, 2*time.Second, 400*time.Millisecond)

	done := make(chan struct{})
	w := httptest.NewRecorder()
	go func() {
		f.p.Serve(context.Background(), w, call(t, true))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("空闲超时没触发：保活帧把计时器一次次推后了，死流永远挂着")
	}

	rec := f.col.record(t)
	if !strings.Contains(rec.ErrorMessage, "idle") {
		t.Errorf("不是空闲超时收尾的：%q", rec.ErrorMessage)
	}
	// 前提确认：这段时间里确实发过保活帧，否则上面那条断言恒成立。
	if !strings.Contains(w.Body.String(), "event: ping") {
		t.Error("这段静默期内一个保活帧都没发，超时断言等于没测")
	}
}

// 非流式客户端不发保活：它的响应是一次性 JSON，中间插字节会把体弄坏。
func TestNoHeartbeatForNonStreamingClient(t *testing.T) {
	up := &fakeUpstream{handler: silentAfterFirstFrame(300 * time.Millisecond)}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})
	tuneOpts(f, 50*time.Millisecond, 2*time.Second, 2*time.Second)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, false))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q，这条应该是一次性 JSON", ct)
	}
	// 断言整个体是一份能解的 JSON，而不是「体里没有 ping」。
	// 后者太弱：回落的保活帧是 SSE 注释（`: keepalive`），里面一个 ping
	// 都没有，混进 JSON 前面照样把体弄坏，而那条断言看不出来。
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体不是一份完整 JSON，保活帧混进去了：%v\n%q", err, w.Body)
	}
	if body["type"] != "message" {
		t.Errorf("响应体结构不对：%v", body)
	}
}

// 首帧之前不发保活：响应头还没写出，此时往 w 写会把状态码钉死在 200，
// 而这个阶段的失败本该换目标重试或回一个正确的 HTTP 错误码。
func TestNoHeartbeatBeforeFirstToken(t *testing.T) {
	up := &fakeUpstream{handler: func(n int, w http.ResponseWriter) {
		if n == 0 {
			// 先静默够几个心跳周期，再回一个可重试的失败。
			time.Sleep(200 * time.Millisecond)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
			return
		}
		writeStream(w, okStream)
	}}
	url := up.start(t)
	f := newFixture(t,
		relaymock.Step{Target: target(url, "kimi-1/k3")},
		relaymock.Step{Target: target(url, "kimi-1/k3")},
	)
	tuneOpts(f, 20*time.Millisecond, 2*time.Second, 2*time.Second)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	// 换目标重试成功。若首帧前发过保活，状态码会被钉在 200 且第一次
	// 那条失败无法重试，这里就拿不到成功的内容。
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if up.calls() != 2 {
		t.Fatalf("上游被打了 %d 次，这条用例要的是首次失败后换目标", up.calls())
	}
	if !strings.Contains(w.Body.String(), "hello") {
		t.Errorf("重试后的内容没交付：%q", w.Body)
	}
	// 客户端看到的第一个字节必须是 message_start，不能是保活帧。
	if i := strings.Index(w.Body.String(), "event: ping"); i >= 0 {
		if j := strings.Index(w.Body.String(), "event: message_start"); i < j {
			t.Errorf("首帧之前发了保活帧：%q", w.Body)
		}
	}
}

// 上游已回 200 与响应头、但在首帧之前静默时也不能发保活。
//
// 与 TestNoHeartbeatBeforeFirstToken 的区别在观察点：那条的静默发生在
// client.Do 里（上游连响应头都没回），此时 bridge 还没开始循环，
// 心跳计时器压根没在跑。这条才真正把「未 committed 的循环里心跳被触发」
// 这个形态摆出来——首帧超时之前的那段等待是本服务最常见的状态。
func TestNoHeartbeatBeforeFirstFrameWhileWaiting(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		// 先只回响应头，一个帧都不发。
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(okStream))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})
	tuneOpts(f, 20*time.Millisecond, 2*time.Second, 2*time.Second)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	out := w.Body.String()
	start := strings.Index(out, "event: message_start")
	if start < 0 {
		t.Fatalf("内容没交付：%q", out)
	}
	// 首帧之前那 200ms 够跑十个心跳周期。客户端看到的第一个字节必须是
	// message_start：此时响应头还没写出，往 w 写会把状态码钉死在 200，
	// 而这个阶段的失败本该换目标重试或回一个正确的 HTTP 错误码。
	if ping := strings.Index(out, "event: ping"); ping >= 0 && ping < start {
		t.Errorf("首帧之前发了保活帧：%q", out)
	}
}

// 真帧必须推进空闲超时，否则长回答会在中途被自己掐断。
//
// 这是绝对时刻那个改动的另一半：只记「起点 + timeout」而不在收到帧时
// 重算的话，任何比空闲超时更长的回答都会失败——而那是最常见的正常情形。
func TestRealFramesKeepStreamAliveBeyondIdleTimeout(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// 每 60ms 发一帧，连发 10 帧：总跨度 600ms 远超 200ms 的空闲超时，
		// 但任何两帧之间的间隔都远小于它。
		frames := []string{
			`event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"kimi-k3-256k","usage":{"input_tokens":100}}}

`,
			`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

`,
		}
		for i := 0; i < 6; i++ {
			frames = append(frames, `event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}

`)
		}
		frames = append(frames,
			`event: content_block_stop
data: {"type":"content_block_stop","index":0}

`,
			`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}

`,
			`event: message_stop
data: {"type":"message_stop"}

`)
		for _, fr := range frames {
			_, _ = w.Write([]byte(fr))
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			time.Sleep(60 * time.Millisecond)
		}
	}}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})
	// 两个超时都设成 200ms：首帧超时也必须短于总跨度，否则整条流在首帧
	// 那个预算内就跑完了，空闲超时的计时器压根没被用到。
	tuneOpts(f, -1, 200*time.Millisecond, 200*time.Millisecond)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	rec := f.col.record(t)
	if rec.Outcome != "normal" {
		t.Fatalf("一个持续出帧的流被掐断了：outcome=%q error=%q",
			rec.Outcome, rec.ErrorMessage)
	}
	if !strings.Contains(w.Body.String(), "message_stop") {
		t.Errorf("流没正常收束：%q", w.Body)
	}
}

// 间隔配成负值时完全不发保活。
func TestHeartbeatDisabledByNegativeInterval(t *testing.T) {
	up := &fakeUpstream{handler: silentAfterFirstFrame(300 * time.Millisecond)}
	url := up.start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})
	tuneOpts(f, -1, 2*time.Second, 2*time.Second)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "event: ping") {
		t.Errorf("显式关闭了保活却还在发：%q", w.Body)
	}
	if !strings.Contains(w.Body.String(), "hello") {
		t.Errorf("内容没交付：%q", w.Body)
	}
}

// 没实现 StreamHeartbeat 的协议回落到 SSE 注释帧。
//
// 四个入站协议里只有 anthropic 有自己的 ping 事件类型，
// 另外三个都走这条回落。逐个跑：回落若被写死成某一个协议的形状，
// 其余的会收到不该收到的东西。
func TestHeartbeatFallsBackToCommentForOtherProtocols(t *testing.T) {
	for _, proto := range []string{
		codec.ProtocolChatCompletions, codec.ProtocolResponses, codec.ProtocolGemini,
	} {
		t.Run(proto, func(t *testing.T) {
			in, ok := codec.Inbound(proto)
			if !ok {
				t.Skipf("%s 没注册入站 codec", proto)
			}
			enc := in.NewStreamEncoder()
			if h, isHB := enc.(codec.StreamHeartbeat); isHB {
				t.Fatalf("%s 自己实现了 StreamHeartbeat，这条回落用例的前提不成立"+
					"（帧 = %q）", proto, h.HeartbeatFrame())
			}
			if !strings.HasPrefix(string(codec.HeartbeatComment), ":") {
				t.Errorf("回落帧不是 SSE 注释，客户端会把它当数据：%q",
					codec.HeartbeatComment)
			}
		})
	}
}

// anthropic 的保活帧是 ping 事件，而不是注释。
func TestAnthropicHeartbeatIsPingEvent(t *testing.T) {
	in, ok := codec.Inbound(codec.ProtocolAnthropic)
	if !ok {
		t.Fatal("anthropic 入站 codec 没注册")
	}
	h, isHB := in.NewStreamEncoder().(codec.StreamHeartbeat)
	if !isHB {
		t.Fatal("anthropic 编码器没实现 StreamHeartbeat")
	}
	got := string(h.HeartbeatFrame())
	if !strings.Contains(got, "event: ping") {
		t.Errorf("保活帧不是 ping 事件：%q", got)
	}
	// 帧里不得带内容语义：ping 之外的任何事件类型都会被客户端当成内容。
	for _, bad := range []string{"content_block", "message_stop", "message_start"} {
		if strings.Contains(got, bad) {
			t.Errorf("保活帧里混进了 %s：%q", bad, got)
		}
	}
}
