package pipeline_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	_ "github.com/aceaura/model-surge-agent/backend/codec/chatcompletions"
	_ "github.com/aceaura/model-surge-agent/backend/codec/gemini"
	_ "github.com/aceaura/model-surge-agent/backend/codec/responses"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
	"github.com/aceaura/model-surge-agent/backend/relaymock"
)

// 上游忽略 stream:true 回的一整份 anthropic 响应。兼容层网关的常见形态。
const wholeAnthropicResponse = `{"id":"msg_whole","type":"message","role":"assistant",
  "model":"kimi-k3-256k","content":[{"type":"text","text":"hello from a whole response"}],
  "stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":7}}`

func writeWhole(w http.ResponseWriter, ct, raw string) {
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(raw))
}

func wholeJSONUpstream(raw string) *fakeUpstream {
	return &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeWhole(w, "application/json", raw)
	}}
}

// 四种流式组合：客户端要不要流 × 上游给不给流。内容必须完整到齐。
func TestClientIntentAndUpstreamShapeMatrix(t *testing.T) {
	cases := []struct {
		name         string
		clientStream bool
		upstreamSSE  bool
		wantText     string
	}{
		{"client stream, upstream sse", true, true, "hello"},
		{"client stream, upstream whole", true, false, "hello from a whole response"},
		{"client buffered, upstream sse", false, true, "hello"},
		{"client buffered, upstream whole", false, false, "hello from a whole response"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sse := c.upstreamSSE
			up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
				if sse {
					writeStream(w, okStream)
					return
				}
				writeWhole(w, "application/json", wholeAnthropicResponse)
			}}
			url := up.start(t)
			f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})

			w := httptest.NewRecorder()
			f.p.Serve(context.Background(), w, call(t, c.clientStream))

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", w.Code, w.Body)
			}
			// 客户端的流式意图不被上游形态改写。
			wantCT := "application/json"
			if c.clientStream {
				wantCT = "text/event-stream"
			}
			if ct := w.Header().Get("Content-Type"); ct != wantCT {
				t.Errorf("content-type = %q, want %q", ct, wantCT)
			}
			if !strings.Contains(w.Body.String(), c.wantText) {
				t.Fatalf("body missing %q:\n%s", c.wantText, w.Body)
			}
			if got := f.col.outcomes(); len(got) != 1 || got[0] != relayclient.OutcomeNormal {
				t.Errorf("outcomes = %v", got)
			}
			rec := f.col.record(t)
			if rec.Attempts != 1 || !rec.Committed {
				t.Errorf("record = %+v", rec)
			}
			// 用量必须来自上游而不是零。
			if rec.Usage.InputTokens != 100 || rec.Usage.OutputTokens != 7 {
				t.Errorf("usage = %+v", rec.Usage)
			}
		})
	}
}

// 流式客户端拿到的是一份内容完整的 SSE，块的生命周期齐全。
func TestWholeResponseIsRenderedAsCompleteSSE(t *testing.T) {
	url := wholeJSONUpstream(wholeAnthropicResponse).start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	out := w.Body.String()
	for _, want := range []string{
		"message_start", "content_block_start", "content_block_delta",
		"hello from a whole response", "content_block_stop",
		"message_delta", "message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	// 终止事件只有一份：投影已带 message_stop，解码器不该再补一个。
	if n := strings.Count(out, `"type":"message_stop"`); n != 1 {
		t.Errorf("message_stop count = %d:\n%s", n, out)
	}
}

// 非流式客户端拿到的整份响应与上游等价。
func TestWholeResponseIsRenderedAsCompleteJSON(t *testing.T) {
	url := wholeJSONUpstream(wholeAnthropicResponse).start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, false))

	var got struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, w.Body)
	}
	if got.ID != "msg_whole" || got.Model != "kimi-k3-256k" {
		t.Errorf("identity = %q/%q", got.ID, got.Model)
	}
	if got.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q", got.StopReason)
	}
	if len(got.Content) != 1 || got.Content[0].Text != "hello from a whole response" {
		t.Fatalf("content = %#v", got.Content)
	}
	if got.Usage.InputTokens != 100 || got.Usage.OutputTokens != 7 {
		t.Errorf("usage = %+v", got.Usage)
	}
}

func hasNote(notes []string, want string) bool {
	for _, n := range notes {
		if n == want {
			return true
		}
	}
	return false
}

func TestUpstreamIgnoringStreamIsRecordedAsLossy(t *testing.T) {
	url := wholeJSONUpstream(wholeAnthropicResponse).start(t)
	f := newFixture(t, relaymock.Step{Target: target(url, "kimi-1/k3")})

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	rec := f.col.record(t)
	if !hasNote(rec.Lossy, codec.UpstreamIgnoredStreamNote) {
		t.Fatalf("lossy = %#v", rec.Lossy)
	}
}

func TestSSEUpstreamProducesNoSuchNote(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	f := newFixture(t, relaymock.Step{Target: target(up.start(t), "kimi-1/k3")})

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	rec := f.col.record(t)
	if hasNote(rec.Lossy, codec.UpstreamIgnoredStreamNote) {
		t.Fatalf("lossy = %#v", rec.Lossy)
	}
}

// 头缺失或解不动时按 SSE 处理：绝大多数缺头的上游发的是正常 SSE，
// 错判成整份响应会把真流缓冲成一整份、毁掉逐字输出。
func TestAmbiguousContentTypeIsTreatedAsSSE(t *testing.T) {
	cases := map[string]string{
		// 空串表示显式发一个空的 Content-Type：不设这个头的话 net/http 会
		// 嗅探出 text/plain，测不到「头真的缺失」这条。
		"missing":     "",
		"unparsable":  "application/",
		"charset sse": "text/event-stream; charset=utf-8",
		"upper sse":   "TEXT/EVENT-STREAM",
	}
	for name, ct := range cases {
		t.Run(name, func(t *testing.T) {
			header := ct
			up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
				w.Header()["Content-Type"] = []string{header}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(okStream))
			}}
			f := newFixture(t, relaymock.Step{Target: target(up.start(t), "kimi-1/k3")})

			w := httptest.NewRecorder()
			f.p.Serve(context.Background(), w, call(t, true))

			if !strings.Contains(w.Body.String(), "hello") {
				t.Fatalf("body = %s", w.Body)
			}
			rec := f.col.record(t)
			if hasNote(rec.Lossy, codec.UpstreamIgnoredStreamNote) {
				t.Errorf("lossy = %#v", rec.Lossy)
			}
		})
	}
}

// 能解出 media type 但不是 text/event-stream 的一律按整份响应处理，
// 包括没听说过的类型：白名单只认 SSE 一项，判不出流就当它不是流。
// 内容真的读不懂时会报上游故障换目标，比伪造一个空答案强。
func TestUnknownMediaTypeIsTreatedAsWhole(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		w.Header()["Content-Type"] = []string{"!!!"}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(wholeAnthropicResponse))
	}}
	f := newFixture(t, relaymock.Step{Target: target(up.start(t), "kimi-1/k3")})

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, false))

	if !strings.Contains(w.Body.String(), "hello from a whole response") {
		t.Fatalf("body = %s", w.Body)
	}
}

// JSON 的各种写法都要被认出来，不能只认字面 application/json。
func TestNonSSEContentTypesAreAllAdopted(t *testing.T) {
	for _, ct := range []string{
		"application/json",
		"application/json; charset=utf-8",
		"APPLICATION/JSON",
		"text/plain",
	} {
		t.Run(ct, func(t *testing.T) {
			header := ct
			up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
				writeWhole(w, header, wholeAnthropicResponse)
			}}
			f := newFixture(t, relaymock.Step{Target: target(up.start(t), "kimi-1/k3")})

			w := httptest.NewRecorder()
			f.p.Serve(context.Background(), w, call(t, false))

			if !strings.Contains(w.Body.String(), "hello from a whole response") {
				t.Fatalf("body = %s", w.Body)
			}
		})
	}
}

// 解不动的整份响应必须报上游故障并换目标，而不是伪造一个空答案。
func TestUndecodableWholeResponseIsRetryable(t *testing.T) {
	bad := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeWhole(w, "application/json", `{"unexpected":`)
	}}
	good := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	f := newFixture(t,
		relaymock.Step{Target: target(bad.start(t), "kimi-1/k3")},
		relaymock.Step{Target: target(good.start(t), "kimi-2/k3")},
	)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "hello") {
		t.Fatalf("body = %s", w.Body)
	}
	rec := f.col.record(t)
	if rec.Attempts != 2 || rec.ModelID != "kimi-2/k3" {
		t.Errorf("record = %+v", rec)
	}
	// 第一个目标要被报成上游故障，否则它会被当成健康的继续接流量。
	if got := f.col.outcomes(); len(got) != 2 || got[0] == relayclient.OutcomeNormal {
		t.Errorf("outcomes = %v", got)
	}
}

// 没有别的目标可换时，解不动的整份响应必须回一个错误，而不是一个
// 语法合法、内容为空的成功响应——那正是本轮修的故障形态。
func TestUndecodableWholeResponseWithNoAlternativeIsAnError(t *testing.T) {
	bad := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeWhole(w, "application/json", `{"unexpected":`)
	}}
	f := newFixture(t, relaymock.Step{Target: target(bad.start(t), "kimi-1/k3")})

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, false))

	if w.Code == http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	rec := f.col.record(t)
	if rec.ErrorCode == "" {
		t.Errorf("record = %+v", rec)
	}
}

// 非 SSE 的空体与「一帧都没发」同口径：都是上游在写内容之前就结束了。
func TestEmptyWholeResponseIsTruncation(t *testing.T) {
	for name, raw := range map[string]string{"empty": "", "whitespace": "  \n\t "} {
		t.Run(name, func(t *testing.T) {
			body := raw
			bad := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
				writeWhole(w, "application/json", body)
			}}
			good := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
				writeStream(w, okStream)
			}}
			f := newFixture(t,
				relaymock.Step{Target: target(bad.start(t), "kimi-1/k3")},
				relaymock.Step{Target: target(good.start(t), "kimi-2/k3")},
			)

			w := httptest.NewRecorder()
			f.p.Serve(context.Background(), w, call(t, true))

			if !strings.Contains(w.Body.String(), "hello") {
				t.Fatalf("body = %s", w.Body)
			}
			rec := f.col.record(t)
			if rec.Attempts != 2 || rec.ModelID != "kimi-2/k3" {
				t.Errorf("record = %+v", rec)
			}
		})
	}
}

// 四个出站协议的整份响应都要被采纳，不能只有 anthropic 走得通。
func TestEveryOutboundProtocolCanAdoptAWholeResponse(t *testing.T) {
	cases := []struct {
		protocol string
		body     string
	}{
		{codec.ProtocolAnthropic, wholeAnthropicResponse},
		{codec.ProtocolChatCompletions, `{"id":"cc_1","model":"kimi-k3-256k",
		  "choices":[{"index":0,"message":{"role":"assistant","content":"whole cc"},
		  "finish_reason":"stop"}],
		  "usage":{"prompt_tokens":100,"completion_tokens":7}}`},
		{codec.ProtocolResponses, `{"id":"resp_1","model":"kimi-k3-256k","status":"completed",
		  "output":[{"type":"message","role":"assistant",
		  "content":[{"type":"output_text","text":"whole responses"}]}],
		  "usage":{"input_tokens":100,"output_tokens":7}}`},
		{codec.ProtocolGemini, `{"candidates":[{"content":{"role":"model",
		  "parts":[{"text":"whole gemini"}]},"finishReason":"STOP"}],
		  "usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":7}}`},
	}
	for _, c := range cases {
		t.Run(c.protocol, func(t *testing.T) {
			if _, ok := codec.Outbound(c.protocol); !ok {
				t.Fatalf("outbound codec %q not registered", c.protocol)
			}
			body := c.body
			up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
				writeWhole(w, "application/json", body)
			}}
			tgt := target(up.start(t), "kimi-1/k3")
			tgt.Protocol = c.protocol
			f := newFixture(t, relaymock.Step{Target: tgt})

			w := httptest.NewRecorder()
			f.p.Serve(context.Background(), w, call(t, false))

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", w.Code, w.Body)
			}
			rec := f.col.record(t)
			if rec.Attempts != 1 || !rec.Committed {
				t.Fatalf("record = %+v", rec)
			}
			if rec.Usage.OutputTokens != 7 {
				t.Errorf("usage = %+v", rec.Usage)
			}
			if !hasNote(rec.Lossy, codec.UpstreamIgnoredStreamNote) {
				t.Errorf("lossy = %#v", rec.Lossy)
			}
			// 内容非空是本轮的要害：缺口的表现正是内容为空。
			if !strings.Contains(w.Body.String(), "whole") &&
				!strings.Contains(w.Body.String(), "hello") {
				t.Fatalf("body = %s", w.Body)
			}
		})
	}
}

// 整份响应里的工具调用要完整到达客户端，包括 id 与入参。
func TestWholeResponseCarriesToolCalls(t *testing.T) {
	raw := `{"id":"msg_tool","type":"message","role":"assistant","model":"kimi-k3-256k",
	  "content":[{"type":"tool_use","id":"toolu_1","name":"read",
	  "input":{"path":"/tmp/a"}}],
	  "stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":3}}`
	f := newFixture(t, relaymock.Step{Target: target(wholeJSONUpstream(raw).start(t), "kimi-1/k3")})

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, false))

	out := w.Body.String()
	for _, want := range []string{"toolu_1", "read", "/tmp/a", "tool_use"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

// 上游给流式而客户端要非流式时，仍按客户端的意图回整份 JSON。
func TestUpstreamShapeDoesNotOverrideClientIntent(t *testing.T) {
	up := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	f := newFixture(t, relaymock.Step{Target: target(up.start(t), "kimi-1/k3")})

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, false))

	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	if strings.Contains(w.Body.String(), "event:") {
		t.Fatalf("body looks like SSE: %s", w.Body)
	}
}

// 超限的整份响应报上游故障，而不是无界读进内存。
func TestOversizedWholeResponseIsUpstreamError(t *testing.T) {
	// 32MiB 上限，构造略超的一份。
	const size = (32 << 20) + 1024
	bad := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"id":"msg_big","type":"message","role":"assistant",
		  "model":"m","content":[{"type":"text","text":%q}],"stop_reason":"end_turn"}`,
			strings.Repeat("x", size))
	}}
	good := &fakeUpstream{handler: func(_ int, w http.ResponseWriter) {
		writeStream(w, okStream)
	}}
	f := newFixture(t,
		relaymock.Step{Target: target(bad.start(t), "kimi-1/k3")},
		relaymock.Step{Target: target(good.start(t), "kimi-2/k3")},
	)

	w := httptest.NewRecorder()
	f.p.Serve(context.Background(), w, call(t, true))

	if !strings.Contains(w.Body.String(), "hello") {
		t.Fatalf("body = %s", w.Body)
	}
	rec := f.col.record(t)
	if rec.Attempts != 2 {
		t.Errorf("record = %+v", rec)
	}
}
