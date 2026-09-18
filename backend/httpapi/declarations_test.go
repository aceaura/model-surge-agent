package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/codec/anthropic"
	"github.com/aceaura/model-surge-agent/backend/codec/chatcompletions"
	"github.com/aceaura/model-surge-agent/backend/codec/gemini"
	"github.com/aceaura/model-surge-agent/backend/codec/responses"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
)

// 这里测的是客户端在头里的**声明**去哪了，以及请求 ID 的可信边界。
// 声明与请求体不同：体的每个字段都有 codec 负责翻译，而声明是传输层元数据,
// 只有 anthropic 出站承载得了。

const anthBody = `{"model":"user-model","max_tokens":64,
  "messages":[{"role":"user","content":"hi"}]}`

// postWithHeader 发一个 anthropic 请求，头用 Add 而非 Set——
// 列表值头要能分多行发，Set 会把前一行覆盖掉。
func postMultiHeader(t *testing.T, f *fixture, name string, values ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(anthBody))
	r.Header.Set("Content-Type", "application/json")
	for _, v := range values {
		r.Header.Add(name, v)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

// --- 需求 1：协议版本声明 ---

func TestInboundAPIVersionReachesUpstream(t *testing.T) {
	f := newFixture(t)
	if w := f.post(t, "/v1/messages", anthBody,
		map[string]string{"anthropic-version": "2024-10-22"}); w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := f.upstream.header("anthropic-version"); got != "2024-10-22" {
		t.Errorf("upstream anthropic-version = %q, want the client's 2024-10-22", got)
	}
}

func TestAbsentAPIVersionFallsBackToDefault(t *testing.T) {
	f := newFixture(t)
	if w := f.post(t, "/v1/messages", anthBody, nil); w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := f.upstream.header("anthropic-version"); got != "2023-06-01" {
		t.Errorf("upstream anthropic-version = %q, want the 2023-06-01 default", got)
	}
}

// TestUnknownAPIVersionIsNotRejected 钉住「不校验版本值」。
// 上游是唯一知道哪些版本有效的一方，拒掉一个我们没听过但它认识的版本，
// 故障就是我们造成的。
func TestUnknownAPIVersionIsNotRejected(t *testing.T) {
	f := newFixture(t)
	if w := f.post(t, "/v1/messages", anthBody,
		map[string]string{"anthropic-version": "9999-01-01"}); w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := f.upstream.header("anthropic-version"); got != "9999-01-01" {
		t.Errorf("upstream anthropic-version = %q, want it forwarded verbatim", got)
	}
}

// TestTargetHeadersWinOverClientDeclarations 钉住合并顺序：
// 调度层的头来自运维配置，运维意图优先于客户端声明。
func TestTargetHeadersWinOverClientDeclarations(t *testing.T) {
	f := newFixtureWithTarget(t, func(tg *relayclient.Target) {
		tg.Headers = map[string]string{
			"x-api-key":         "sk-upstream",
			"anthropic-version": "2099-12-31",
		}
	})
	if w := f.post(t, "/v1/messages", anthBody,
		map[string]string{"anthropic-version": "2024-10-22"}); w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := f.upstream.header("anthropic-version"); got != "2099-12-31" {
		t.Errorf("upstream anthropic-version = %q, want the dispatcher's 2099-12-31", got)
	}
}

// --- 需求 2：特性声明 ---

func TestBetaTokensReachUpstream(t *testing.T) {
	f := newFixture(t)
	if w := f.post(t, "/v1/messages", anthBody,
		map[string]string{"anthropic-beta": "prompt-caching-2024-07-31"}); w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := f.upstream.header("anthropic-beta"); got != "prompt-caching-2024-07-31" {
		t.Errorf("upstream anthropic-beta = %q", got)
	}
}

// TestBetaTokensFromMultipleHeaderLines 钉住用 Header.Values 而非 Get。
// anthropic-beta 是列表值头，客户端可以分多行发；用 Get 只会拿到第一行。
func TestBetaTokensFromMultipleHeaderLines(t *testing.T) {
	f := newFixture(t)
	if w := postMultiHeader(t, f, "anthropic-beta", "first-2025-01-01", "second-2025-02-02"); w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := f.upstream.header("anthropic-beta"); got != "first-2025-01-01,second-2025-02-02" {
		t.Errorf("upstream anthropic-beta = %q, want both header lines", got)
	}
}

func TestBetaTokensAreSplitAndTrimmed(t *testing.T) {
	f := newFixture(t)
	if w := f.post(t, "/v1/messages", anthBody,
		map[string]string{"anthropic-beta": " a-1 , , b-2 "}); w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := f.upstream.header("anthropic-beta"); got != "a-1,b-2" {
		t.Errorf("upstream anthropic-beta = %q, want trimmed with the empty token dropped", got)
	}
}

// TestBetaTokensAreDedupedKeepingFirstOrder 顺序是客户端的表达，
// 重排没有收益却让对账变难。
func TestBetaTokensAreDedupedKeepingFirstOrder(t *testing.T) {
	f := newFixture(t)
	if w := f.post(t, "/v1/messages", anthBody,
		map[string]string{"anthropic-beta": "z-1,a-2,z-1,m-3"}); w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := f.upstream.header("anthropic-beta"); got != "z-1,a-2,m-3" {
		t.Errorf("upstream anthropic-beta = %q, want dedupe in first-seen order", got)
	}
}

func TestUnknownBetaTokenIsNotRejected(t *testing.T) {
	f := newFixture(t)
	if w := f.post(t, "/v1/messages", anthBody,
		map[string]string{"anthropic-beta": "not-a-real-token-9999"}); w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := f.upstream.header("anthropic-beta"); got != "not-a-real-token-9999" {
		t.Errorf("upstream anthropic-beta = %q, want it forwarded verbatim", got)
	}
}

// TestNoServiceOwnedBetaTokenIsInjected 钉住不做身份伪装。
// cc-switch 与 sub2api 都注入 Claude Code 的令牌以通过上游指纹检查；
// 要那么做应当由运维在调度层的账号头里配，不该由数据面代劳。
func TestNoServiceOwnedBetaTokenIsInjected(t *testing.T) {
	f := newFixture(t)
	if w := f.post(t, "/v1/messages", anthBody, nil); w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if f.upstream.hasHeader("anthropic-beta") {
		t.Errorf("upstream anthropic-beta = %q, want no header at all when the client declared none",
			f.upstream.header("anthropic-beta"))
	}
	for _, h := range []string{"User-Agent", "X-App"} {
		if v := f.upstream.header(h); strings.Contains(strings.ToLower(v), "claude") {
			t.Errorf("upstream %s = %q looks like client impersonation", h, v)
		}
	}
}

// --- 需求 3：只发给能承载声明的出站协议 ---

// TestOnlyAnthropicCarriesDeclarations 是结构守卫。
//
// 用「实现了接口吗」而不是在公共路径判协议名：判断会在新增出站协议时
// 被漏掉，漏了就是把 Anthropic 的头发给不认识它的上游；不实现接口不会。
func TestOnlyAnthropicCarriesDeclarations(t *testing.T) {
	want := map[string]bool{
		anthropic.Name:       true,
		chatcompletions.Name: false,
		responses.Name:       false,
		gemini.Name:          false,
	}
	for name, carries := range want {
		out, ok := codec.Outbound(name)
		if !ok {
			t.Fatalf("outbound %s not registered", name)
		}
		if _, got := out.(codec.DeclarationEncoder); got != carries {
			t.Errorf("%s implements DeclarationEncoder = %v, want %v", name, got, carries)
		}
	}
}

func TestNonAnthropicOutboundGetsNoDeclarationHeaders(t *testing.T) {
	decls := codec.Declarations{APIVersion: "2024-10-22", Betas: []string{"a-1"}}
	for _, name := range []string{chatcompletions.Name, responses.Name, gemini.Name} {
		t.Run(name, func(t *testing.T) {
			out, _ := codec.Outbound(name)
			if de, ok := out.(codec.DeclarationEncoder); ok {
				t.Fatalf("%s must not carry declarations, got %v", name, de.DeclarationHeaders(decls))
			}
			_, extra := out.Endpoint("https://x.test", "native", true)
			for k := range extra {
				if strings.HasPrefix(strings.ToLower(k), "anthropic-") {
					t.Errorf("%s endpoint emits %q", name, k)
				}
			}
		})
	}
}

func TestDroppedDeclarationsAreRecordedAsLossy(t *testing.T) {
	cases := []struct {
		name  string
		decls codec.Declarations
		want  []string
	}{
		{"version only", codec.Declarations{APIVersion: "2024-10-22"}, []string{"anthropic-version"}},
		{"betas only", codec.Declarations{Betas: []string{"a-1"}}, []string{"anthropic-beta"}},
		{"both", codec.Declarations{APIVersion: "2024-10-22", Betas: []string{"a-1"}},
			[]string{"anthropic-version", "anthropic-beta"}},
	}
	out, _ := codec.Outbound(chatcompletions.Name)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			notes := codec.DescribeDeclarationLoss(c.decls, out)
			if len(notes) != len(c.want) {
				t.Fatalf("notes = %v, want %d of them", notes, len(c.want))
			}
			for _, prefix := range c.want {
				found := false
				for _, n := range notes {
					if strings.HasPrefix(n, prefix) {
						found = true
					}
				}
				if !found {
					t.Errorf("notes = %v, missing one about %s", notes, prefix)
				}
			}
		})
	}
}

// TestEmptyDeclarationsProduceNoLossyNote：客户端没声明就没有损失。
// 报了会让每条非 anthropic 出站的流水都挂一条噪声。
func TestEmptyDeclarationsProduceNoLossyNote(t *testing.T) {
	for _, name := range []string{anthropic.Name, chatcompletions.Name, responses.Name, gemini.Name} {
		out, _ := codec.Outbound(name)
		if notes := codec.DescribeDeclarationLoss(codec.Declarations{}, out); notes != nil {
			t.Errorf("%s: notes = %v, want none", name, notes)
		}
	}
}

// TestAnthropicOutboundReportsNoDeclarationLoss：承载得了就不该报有损。
func TestAnthropicOutboundReportsNoDeclarationLoss(t *testing.T) {
	out, _ := codec.Outbound(anthropic.Name)
	decls := codec.Declarations{APIVersion: "2024-10-22", Betas: []string{"a-1"}}
	if notes := codec.DescribeDeclarationLoss(decls, out); notes != nil {
		t.Errorf("notes = %v, want none", notes)
	}
}

func TestLossyDeclarationNoteReachesTheRequestLog(t *testing.T) {
	// 让调度层给一个 chat_completions 目标：该协议承载不了 Anthropic 的声明。
	f := newFixtureWithTarget(t, func(tg *relayclient.Target) {
		tg.Protocol = codec.ProtocolChatCompletions
	})
	f.post(t, "/v1/messages", anthBody, map[string]string{"anthropic-beta": "a-1"})
	rec := f.records.one(t)
	found := false
	for _, n := range rec.Lossy {
		if strings.HasPrefix(n, "anthropic-beta") {
			found = true
		}
	}
	if !found {
		t.Errorf("record lossy = %v, want a note about the dropped beta declaration", rec.Lossy)
	}
}

// --- 需求 4：请求 ID 的可信边界 ---

func TestClientRequestIDIsHonoredWhenValid(t *testing.T) {
	for _, id := range []string{
		"req_abc123",
		"550e8400-e29b-41d4-a716-446655440000",
		"trace:span.1_2",
		strings.Repeat("a", 128), // 正好等于上限，必须放行
	} {
		f := newFixture(t)
		w := f.post(t, "/v1/messages", anthBody, map[string]string{"X-Request-Id": id})
		if got := w.Header().Get("X-Request-Id"); got != id {
			t.Errorf("echoed id = %q, want the client's %q", got, id)
		}
		if got := f.records.one(t).RequestID; got != id {
			t.Errorf("recorded id = %q, want %q", got, id)
		}
	}
}

// TestOverlongRequestIDIsReplaced 钉住长度上限的两侧。
// 无上限的后果：这个值会进 request_log 主键、响应头与上报 ID。
func TestOverlongRequestIDIsReplaced(t *testing.T) {
	long := strings.Repeat("a", 129)
	f := newFixture(t)
	w := f.post(t, "/v1/messages", anthBody, map[string]string{"X-Request-Id": long})
	if got := w.Header().Get("X-Request-Id"); got == long {
		t.Fatalf("a 129-byte id was honored")
	} else if len(got) == 0 {
		t.Fatalf("no id was generated")
	}
}

// TestIllegalRequestIDCharactersAreReplaced 是主键污染的直接断言：
// request_log 的写入是 ON CONFLICT DO UPDATE，客户端发一个已存在的 id
// 就能改掉别人那一行，所以字符集必须收紧。
func TestIllegalRequestIDCharactersAreReplaced(t *testing.T) {
	for name, bad := range map[string]string{
		"space":     "a b",
		"slash":     "a/../b",
		"nul":       "a\x00b",
		"newline":   "a\ndef",
		"cr":        "a\rdef",
		"non-ascii": "a—b",
		"semicolon": "a;b",
		"percent":   "a%20b",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			w := f.post(t, "/v1/messages", anthBody, map[string]string{"X-Request-Id": bad})
			got := w.Header().Get("X-Request-Id")
			if got == bad {
				t.Fatalf("illegal id %q was honored", bad)
			}
			// 不能只是清洗后回显：那还是把客户端的输入写进了响应与日志。
			// 判据是「新生成的」而非「不含客户端的字符」——生成的 id 是
			// 十六进制，本来就会含 a、b 这些字节。
			if !strings.HasPrefix(got, "req_") {
				t.Errorf("echoed id = %q, want a freshly generated one", got)
			}
			if rid := f.records.one(t).RequestID; rid != got {
				t.Errorf("recorded id = %q but echoed %q", rid, got)
			}
		})
	}
}

// TestIllegalRequestIDStillServesTheRequest：客户端的业务请求本身没问题，
// 拒绝会把一个可自愈的卫生问题变成故障。
func TestIllegalRequestIDStillServesTheRequest(t *testing.T) {
	f := newFixture(t)
	w := f.post(t, "/v1/messages", anthBody, map[string]string{"X-Request-Id": "bad id here"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if f.upstreamCalls() != 1 {
		t.Errorf("upstream calls = %d, want 1", f.upstreamCalls())
	}
}

// TestRecordedRequestIDIsAlwaysWellFormed 覆盖长度与字符两条退路。
func TestRecordedRequestIDIsAlwaysWellFormed(t *testing.T) {
	for _, bad := range []string{
		strings.Repeat("x", 4096),
		"a b\tc",
		"",
		"   ",
	} {
		f := newFixture(t)
		f.post(t, "/v1/messages", anthBody, map[string]string{"X-Request-Id": bad})
		id := f.records.one(t).RequestID
		if id == "" || len(id) > 128 {
			t.Errorf("recorded id = %q (len %d) for input %q", id, len(id), bad)
		}
		for i := 0; i < len(id); i++ {
			c := id[i]
			ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
				c == '-' || c == '_' || c == '.' || c == ':'
			if !ok {
				t.Errorf("recorded id = %q has illegal byte %q", id, c)
				break
			}
		}
	}
}

// TestCountTokensValidatesTheRequestID：count_tokens 走同一个 requestID，
// 但它是另一条处理函数，漏改不会被数据面的用例发现。
func TestCountTokensValidatesTheRequestID(t *testing.T) {
	f := newFixture(t)
	w := f.post(t, "/v1/messages/count_tokens", anthBody,
		map[string]string{"X-Request-Id": "bad id here"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Request-Id"); got == "bad id here" {
		t.Errorf("count_tokens echoed the illegal id %q", got)
	}
}
