package httpapi_test

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// 这里测的是「请求还没走到 codec 解码」那一层：路径、方法、请求体边界、
// 传输编码、响应头、以及这些拒绝有没有进流水。
//
// 这一层的故障有个共同特征：请求根本没进协议转换，所以 sanitized / lossy /
// 换目标重试那套诊断一条都不触发。不在这里测，就没有任何地方测。

// --- 需求 1：受理面拒绝用客户端协议的信封 ---

// TestUnknownPathAnswersInTheClientProtocolShape 未知路径也要回协议信封。
//
// 标准库默认回 `404 page not found` 纯文本，SDK 会把它当成解码失败而不是
// 业务错误——客户端开发者看到的是一个 JSON 语法异常，而真正的问题是
// base_url 配错了。
func TestUnknownPathAnswersInTheClientProtocolShape(t *testing.T) {
	cases := []struct {
		name string
		path string
		// wantKey 是该协议错误信封的标志性字段。
		wantKey string
	}{
		{"anthropic by suffix", "/v2/messages", `"type":"error"`},
		{"chat completions by suffix", "/v2/chat/completions", `"error"`},
		{"responses by suffix", "/v2/responses", `"error"`},
		{"openai by prefix", "/openai/v9/nope", `"error"`},
		// 推断不出协议时取 anthropic：数据面必须回一个能被解析的东西。
		{"unrecognisable falls back", "/totally/unknown", `"type":"error"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			resp := f.post(t, c.path, `{}`, nil)
			if resp.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404: %s", resp.Code, resp.Body.String())
			}
			body := resp.Body.String()
			if !strings.Contains(body, c.wantKey) {
				t.Errorf("body must carry %s: %s", c.wantKey, body)
			}
			if !json.Valid(resp.Body.Bytes()) {
				t.Errorf("body must be valid JSON, got: %s", body)
			}
			// 消息里要给出方法与路径，客户端据此能直接看出是 base_url 配错。
			if !strings.Contains(body, c.path) {
				t.Errorf("message must name the rejected path: %s", body)
			}
			if got := resp.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
				t.Errorf("content-type = %q, want json", got)
			}
		})
	}
}

// TestWrongMethodKeepsTheAllowHeaderAndUsesTheProtocolShape 405 要同时满足两件事。
//
// Allow 头是 http.ServeMux 免费给的，三个参考实现一个都没有。改写响应体时
// 最容易把它一起丢掉——那样客户端就不知道该换成哪个方法。
func TestWrongMethodKeepsTheAllowHeaderAndUsesTheProtocolShape(t *testing.T) {
	f := newFixture(t)
	resp := f.get(t, "/v1/messages")
	if resp.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405: %s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Allow"); got == "" {
		t.Error("the Allow header must survive the envelope rewrite; without it the client cannot tell which method to use")
	}
	body := resp.Body.String()
	if !json.Valid(resp.Body.Bytes()) {
		t.Fatalf("body must be valid JSON: %s", body)
	}
	if !strings.Contains(body, `"type":"error"`) {
		t.Errorf("body must use the anthropic error shape: %s", body)
	}
	if !strings.Contains(body, http.MethodGet) {
		t.Errorf("message must name the rejected method: %s", body)
	}
}

// TestAdminRejectionsAreNotRewritten 管理面有自己的错误形状。
//
// 套上数据面的信封会让管理客户端解析失败——它认的是 {error:{code,message}}，
// 不是 anthropic 的 {type,error}。
func TestAdminRejectionsAreNotRewritten(t *testing.T) {
	f := newFixture(t)
	resp := f.get(t, "/admin/nope")
	if strings.Contains(resp.Body.String(), `"type":"error"`) {
		t.Errorf("admin rejections must not get the data-plane envelope: %s", resp.Body.String())
	}
	if len(f.records.all()) != 0 {
		t.Error("admin rejections must not enter the data-plane request log")
	}
}

// --- 需求 2：请求体边界 ---

func TestOversizeBodyIsRejectedAsTooLarge(t *testing.T) {
	// 回 413 而非 400：codec.KindForStatus 把上游的 413 归为 context_exceeded，
	// 本服务自己产生同类错误时走同一套语义，客户端才知道该裁剪输入。
	const limit = 512
	f := newFixture(t, limit)
	huge := fmt.Sprintf(`{"model":"user-model","max_tokens":64,"messages":[{"role":"user","content":%q}]}`,
		strings.Repeat("x", limit*4))

	resp := f.post(t, "/v1/messages", huge, nil)
	if resp.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", resp.Code, resp.Body.String())
	}
	// 消息里带上限值，客户端据此能自己分片而不必二分试。
	if !strings.Contains(resp.Body.String(), fmt.Sprint(limit)) {
		t.Errorf("message must state the limit so the client can split its input: %s", resp.Body.String())
	}
	rec := f.records.one(t)
	if rec.ErrorCode != string(ir.ErrContextExceeded) {
		t.Errorf("error_code = %q, want %q", rec.ErrorCode, ir.ErrContextExceeded)
	}
	if f.upstreamCalls() != 0 {
		t.Error("an oversize body must not reach the upstream")
	}
}

// TestBodyExactlyAtTheLimitIsAccepted 钉住边界的哪一侧。
//
// 「超过上限」必须是严格大于。差一字节判错的后果是不对称的：把合法请求
// 拒掉是客户端永远无法自行修复的故障（它算出来的长度就是上限），
// 而多放一个字节什么也不会发生。压缩路径单独走一遍：那里的长度是从
// LimitReader 读出来的，与非压缩路径不是同一段代码。
func TestBodyExactlyAtTheLimitIsAccepted(t *testing.T) {
	body := fmt.Sprintf(`{"model":"user-model","max_tokens":64,"messages":[{"role":"user","content":%q}]}`,
		strings.Repeat("a", 4000))
	limit := int64(len(body))

	t.Run("plain at the limit", func(t *testing.T) {
		f := newFixture(t, limit)
		if resp := f.postRaw(t, http.MethodPost, "/v1/messages", []byte(body), nil); resp.Code != http.StatusOK {
			t.Errorf("status = %d, want 200: %s", resp.Code, resp.Body.String())
		}
	})
	t.Run("plain one byte over", func(t *testing.T) {
		f := newFixture(t, limit-1)
		if resp := f.postRaw(t, http.MethodPost, "/v1/messages", []byte(body), nil); resp.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want 413", resp.Code)
		}
	})
	t.Run("decompressed at the limit", func(t *testing.T) {
		f := newFixture(t, limit)
		resp := f.postRaw(t, http.MethodPost, "/v1/messages", gzipBytes([]byte(body)),
			map[string]string{"Content-Encoding": "gzip"})
		if resp.Code != http.StatusOK {
			t.Errorf("status = %d, want 200: %s", resp.Code, resp.Body.String())
		}
	})
	t.Run("decompressed one byte over", func(t *testing.T) {
		f := newFixture(t, limit-1)
		resp := f.postRaw(t, http.MethodPost, "/v1/messages", gzipBytes([]byte(body)),
			map[string]string{"Content-Encoding": "gzip"})
		if resp.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want 413: %s", resp.Code, resp.Body.String())
		}
	})
}

func TestEmptyBodySaysSo(t *testing.T) {
	// 透出 json 的 "unexpected end of JSON input" 时，读的人分不清是自己
	// 没发 body 还是 body 被中间层吃了。
	f := newFixture(t)
	for _, body := range []string{"", "   ", "\n\t "} {
		resp := f.postRaw(t, http.MethodPost, "/v1/messages", []byte(body), nil)
		if resp.Code != http.StatusBadRequest {
			t.Fatalf("body %q: status = %d, want 400: %s", body, resp.Code, resp.Body.String())
		}
		if !strings.Contains(strings.ToLower(resp.Body.String()), "empty") {
			t.Errorf("body %q: message must say the body is empty, got: %s", body, resp.Body.String())
		}
	}
}

// --- 需求 3：入站传输编码 ---

func TestCompressedBodyIsAccepted(t *testing.T) {
	// 不解压的话请求体会落到 json.Unmarshal 报 "invalid character"，
	// 一条完全指错方向的消息。
	cases := []struct {
		encoding string
		compress func([]byte) []byte
	}{
		{"gzip", gzipBytes},
		{"deflate", deflateBytes},
	}
	for _, c := range cases {
		t.Run(c.encoding, func(t *testing.T) {
			f := newFixture(t)
			resp := f.postRaw(t, http.MethodPost, "/v1/messages",
				c.compress([]byte(requestBodies[codec.ProtocolAnthropic])),
				map[string]string{"Content-Encoding": c.encoding})
			if resp.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body.String())
			}
			if disp := f.relay.Dispatches(); len(disp) != 1 {
				t.Fatalf("dispatches = %d, want 1 — the decompressed body must reach the dispatcher", len(disp))
			}
		})
	}
}

func TestIdentityAndAbsentEncodingAreUnchanged(t *testing.T) {
	for _, encoding := range []string{"", "identity", "IDENTITY"} {
		f := newFixture(t)
		headers := map[string]string{}
		if encoding != "" {
			headers["Content-Encoding"] = encoding
		}
		resp := f.postRaw(t, http.MethodPost, "/v1/messages",
			[]byte(requestBodies[codec.ProtocolAnthropic]), headers)
		if resp.Code != http.StatusOK {
			t.Errorf("encoding %q: status = %d, want 200: %s", encoding, resp.Code, resp.Body.String())
		}
	}
}

func TestUnsupportedContentEncodingIsRejected(t *testing.T) {
	// 不静默当未压缩处理：那样客户端只会收到一条 JSON 语法错误。
	for _, encoding := range []string{"br", "zstd", "compress"} {
		f := newFixture(t)
		resp := f.postRaw(t, http.MethodPost, "/v1/messages",
			[]byte(requestBodies[codec.ProtocolAnthropic]),
			map[string]string{"Content-Encoding": encoding})
		if resp.Code != http.StatusBadRequest {
			t.Fatalf("encoding %q: status = %d, want 400: %s", encoding, resp.Code, resp.Body.String())
		}
		body := resp.Body.String()
		// 要列出支持的编码，否则客户端只知道"不行"、不知道该换成什么。
		for _, want := range []string{"gzip", "deflate"} {
			if !strings.Contains(body, want) {
				t.Errorf("encoding %q: message must list %s as accepted: %s", encoding, want, body)
			}
		}
	}
}

// TestCompressionCannotBypassTheSizeLimit 是本轮最该有的一条。
//
// 只限压缩前等于没限：几百 KB 的 gzip 能解出几百 MB，而内存是按解压后的
// 大小吃掉的。这个用例的压缩体远小于上限、解压后远超上限。
func TestCompressionCannotBypassTheSizeLimit(t *testing.T) {
	const limit = 4096
	f := newFixture(t, limit)

	// 高度可压缩的 payload：同一个字符重复，gzip 后只剩几百字节。
	plain := fmt.Sprintf(`{"model":"user-model","max_tokens":64,"messages":[{"role":"user","content":%q}]}`,
		strings.Repeat("A", limit*64))
	packed := gzipBytes([]byte(plain))
	if int64(len(packed)) >= limit {
		t.Fatalf("test payload is not compressible enough: packed = %d, limit = %d — "+
			"it must slip past the pre-decompression check to exercise the post-decompression one",
			len(packed), limit)
	}

	resp := f.postRaw(t, http.MethodPost, "/v1/messages", packed,
		map[string]string{"Content-Encoding": "gzip"})
	if resp.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 — a compressed body must not bypass the limit: %s",
			resp.Code, resp.Body.String())
	}
	if rec := f.records.one(t); rec.ErrorCode != string(ir.ErrContextExceeded) {
		t.Errorf("error_code = %q, want %q", rec.ErrorCode, ir.ErrContextExceeded)
	}
}

func TestCorruptCompressedBodyIsAClientError(t *testing.T) {
	// 回 400 而非 500：压缩流损坏是客户端发来的数据有问题，
	// 记成本服务出错会让排查从一开始就走错方向。
	packed := gzipBytes([]byte(requestBodies[codec.ProtocolAnthropic]))
	cases := []struct {
		name string
		body []byte
	}{
		{"truncated", packed[:len(packed)/2]},
		{"not compressed at all", []byte(requestBodies[codec.ProtocolAnthropic])},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			resp := f.postRaw(t, http.MethodPost, "/v1/messages", c.body,
				map[string]string{"Content-Encoding": "gzip"})
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (not 500 — the client sent bad data): %s",
					resp.Code, resp.Body.String())
			}
		})
	}
}

func TestBOMPrefixedBodyIsAccepted(t *testing.T) {
	// encoding/json 不接受 BOM 前缀，而一些 Windows 上的客户端会带它。
	// 不剥就是一条查不出原因的 400。
	f := newFixture(t)
	body := append([]byte{0xEF, 0xBB, 0xBF}, requestBodies[codec.ProtocolAnthropic]...)
	resp := f.postRaw(t, http.MethodPost, "/v1/messages", body, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
}

// --- 需求 4：请求 ID 回给客户端 ---

func TestRequestIDIsEchoedInTheResponse(t *testing.T) {
	// 客户端报障时说"我下午三点那个请求失败了"，没有这个头运维就无从定位。
	// 三种响应形态都要带：它必须在写响应头之前设好。
	t.Run("successful non-streaming", func(t *testing.T) {
		f := newFixture(t)
		resp := f.post(t, "/v1/messages", requestBodies[codec.ProtocolAnthropic], nil)
		assertRequestIDEchoed(t, resp.Header().Get("X-Request-Id"), resp.Code)
	})

	t.Run("streaming", func(t *testing.T) {
		f := newFixture(t)
		body := `{"model":"user-model","max_tokens":64,"stream":true,
		  "messages":[{"role":"user","content":"hi"}]}`
		resp := f.post(t, "/v1/messages", body, nil)
		assertRequestIDEchoed(t, resp.Header().Get("X-Request-Id"), resp.Code)
	})

	t.Run("count_tokens", func(t *testing.T) {
		f := newFixture(t)
		resp := f.post(t, "/v1/messages/count_tokens", requestBodies[codec.ProtocolAnthropic], nil)
		assertRequestIDEchoed(t, resp.Header().Get("X-Request-Id"), resp.Code)
	})

	t.Run("admission rejection", func(t *testing.T) {
		f := newFixture(t)
		resp := f.post(t, "/v1/messages", `{"max_tokens":1}`, nil)
		if resp.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.Code)
		}
		if resp.Header().Get("X-Request-Id") == "" {
			t.Error("rejections need the id too — that is exactly when the client reports a problem")
		}
	})

	t.Run("routing rejection", func(t *testing.T) {
		f := newFixture(t)
		resp := f.post(t, "/nope", `{}`, nil)
		if resp.Header().Get("X-Request-Id") == "" {
			t.Error("404 must carry the request id")
		}
	})
}

func TestClientSuppliedRequestIDIsEchoedUnchanged(t *testing.T) {
	// 回显客户端自己给的值，而不是另生成一个：否则它日志里的 id 与
	// 响应头里的 id 对不上，两边都查不到。
	f := newFixture(t)
	resp := f.post(t, "/v1/messages", requestBodies[codec.ProtocolAnthropic],
		map[string]string{"X-Request-Id": "trace-abc"})
	if got := resp.Header().Get("X-Request-Id"); got != "trace-abc" {
		t.Errorf("X-Request-Id = %q, want the client's own value trace-abc", got)
	}
}

func assertRequestIDEchoed(t *testing.T, id string, status int) {
	t.Helper()
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if id == "" {
		t.Error("X-Request-Id must be echoed so the client can quote it when reporting a problem")
	}
}

// --- 需求 6：流水记请求路径 ---

func TestRequestPathIsRecorded(t *testing.T) {
	// 同一协议的多个别名共用一个处理函数。不记路径就看不出客户端把
	// base_url 配成了哪一种，别名相关的接入问题无从定位。
	for _, path := range []string{"/v1/messages", "/v1/v1/messages", "/anthropic/v1/messages", "/messages"} {
		t.Run(path, func(t *testing.T) {
			f := newFixture(t)
			if resp := f.post(t, path, requestBodies[codec.ProtocolAnthropic], nil); resp.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
			}
			if rec := f.records.one(t); rec.Path != path {
				t.Errorf("path = %q, want %q", rec.Path, path)
			}
		})
	}
}

func TestRecordedPathExcludesQuery(t *testing.T) {
	// 数据面不读任何 query 参数，而一些客户端会把凭据塞进去。
	// 不记就不需要脱敏。
	f := newFixture(t)
	f.post(t, "/v1/messages?key=sk-secret", requestBodies[codec.ProtocolAnthropic], nil)
	rec := f.records.one(t)
	if strings.Contains(rec.Path, "sk-secret") || strings.Contains(rec.Path, "?") {
		t.Errorf("path must not carry the query string: %q", rec.Path)
	}
	if rec.Path != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", rec.Path)
	}
}

// --- 需求 7：受理面拒绝也要进流水 ---

// TestAdmissionRejectionsAreRecorded 这是本轮最实质的一条。
//
// 改动前受理面的每条拒绝路径都是「回错 + return」，Recorder 一次都不调。
// 客户端报「连不上」时，服务端流水里什么都没有，无法判断请求有没有到达。
func TestAdmissionRejectionsAreRecorded(t *testing.T) {
	packed := gzipBytes([]byte(requestBodies[codec.ProtocolAnthropic]))
	cases := []struct {
		name      string
		method    string
		path      string
		body      []byte
		headers   map[string]string
		maxBody   int64
		wantCode  ir.ErrorKind
		wantHTTP  int
		wantPath  string
		wantProto string
	}{
		{
			name: "unknown path", method: http.MethodPost, path: "/v9/messages",
			body: []byte(`{}`), wantCode: ir.ErrNotFound, wantHTTP: http.StatusNotFound,
			wantPath: "/v9/messages", wantProto: codec.ProtocolAnthropic,
		},
		{
			name: "method not allowed", method: http.MethodGet, path: "/v1/messages",
			wantCode: ir.ErrInvalidRequest, wantHTTP: http.StatusMethodNotAllowed,
			wantPath: "/v1/messages", wantProto: codec.ProtocolAnthropic,
		},
		{
			name: "oversize body", method: http.MethodPost, path: "/v1/messages",
			body: []byte(strings.Repeat("x", 4096)), maxBody: 256,
			wantCode: ir.ErrContextExceeded, wantHTTP: http.StatusRequestEntityTooLarge,
			wantPath: "/v1/messages", wantProto: codec.ProtocolAnthropic,
		},
		{
			name: "unsupported encoding", method: http.MethodPost, path: "/v1/chat/completions",
			body: packed, headers: map[string]string{"Content-Encoding": "br"},
			wantCode: ir.ErrInvalidRequest, wantHTTP: http.StatusBadRequest,
			wantPath: "/v1/chat/completions", wantProto: codec.ProtocolChatCompletions,
		},
		{
			name: "corrupt compressed body", method: http.MethodPost, path: "/v1/messages",
			body: packed[:8], headers: map[string]string{"Content-Encoding": "gzip"},
			wantCode: ir.ErrInvalidRequest, wantHTTP: http.StatusBadRequest,
			wantPath: "/v1/messages", wantProto: codec.ProtocolAnthropic,
		},
		{
			name: "empty body", method: http.MethodPost, path: "/v1/responses",
			body: nil, wantCode: ir.ErrInvalidRequest, wantHTTP: http.StatusBadRequest,
			wantPath: "/v1/responses", wantProto: codec.ProtocolResponses,
		},
		{
			name: "undecodable body", method: http.MethodPost, path: "/v1/messages",
			body: []byte(`{not json`), wantCode: ir.ErrInvalidRequest, wantHTTP: http.StatusBadRequest,
			wantPath: "/v1/messages", wantProto: codec.ProtocolAnthropic,
		},
		{
			name: "missing model", method: http.MethodPost, path: "/v1/messages",
			body:     []byte(`{"max_tokens":64,"messages":[]}`),
			wantCode: ir.ErrInvalidRequest, wantHTTP: http.StatusBadRequest,
			wantPath: "/v1/messages", wantProto: codec.ProtocolAnthropic,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, c.maxBody)
			resp := f.postRaw(t, c.method, c.path, c.body, c.headers)
			if resp.Code != c.wantHTTP {
				t.Fatalf("status = %d, want %d: %s", resp.Code, c.wantHTTP, resp.Body.String())
			}

			rec := f.records.one(t)
			if rec.ErrorCode != string(c.wantCode) {
				t.Errorf("error_code = %q, want %q", rec.ErrorCode, c.wantCode)
			}
			if rec.StatusCode != c.wantHTTP {
				t.Errorf("status_code = %d, want %d", rec.StatusCode, c.wantHTTP)
			}
			if rec.Path != c.wantPath {
				t.Errorf("path = %q, want %q", rec.Path, c.wantPath)
			}
			if rec.InboundProtocol != c.wantProto {
				t.Errorf("inbound_protocol = %q, want %q", rec.InboundProtocol, c.wantProto)
			}
			// attempts 记 0 而非 1：运维据此把「从未打上游」与
			// 「打了一次但失败」分开。
			if rec.Attempts != 0 {
				t.Errorf("attempts = %d, want 0 — nothing was ever sent upstream", rec.Attempts)
			}
			if rec.RequestID == "" {
				t.Error("request_id must be set so the client's copy of it can be looked up")
			}
			if rec.ErrorMessage == "" {
				t.Error("error_message must explain why the request was refused")
			}
		})
	}
}

func TestAdmissionRejectionsCarryNoTarget(t *testing.T) {
	// 这一刻还没选过目标。硬编一个占位账号会让某个真实账号无端累计失败。
	f := newFixture(t)
	f.post(t, "/v1/messages", `{"max_tokens":1}`, nil)
	rec := f.records.one(t)
	if rec.ModelID != "" || rec.Account != "" || rec.OutboundProtocol != "" {
		t.Errorf("target fields must stay empty, got model_id=%q account=%q outbound=%q",
			rec.ModelID, rec.Account, rec.OutboundProtocol)
	}
	if len(rec.TriedIDs) != 0 {
		t.Errorf("tried_ids must be empty: %v", rec.TriedIDs)
	}
}

func TestAdmissionRejectionsAreNotReported(t *testing.T) {
	// 上报要求 model_id 与 account，而受理面拒绝时两者都没有。
	// 这与客户端取消记 normal 是同一个判据：不是目标的故障就不记到目标头上。
	f := newFixture(t)
	f.post(t, "/v1/messages", `{"max_tokens":1}`, nil)
	f.post(t, "/nope", `{}`, nil)
	f.get(t, "/v1/messages")
	if got := f.relay.Reports(); len(got) != 0 {
		t.Errorf("reports = %d, want 0 — there is no target to attribute these to: %+v", len(got), got)
	}
	if got := f.relay.Dispatches(); len(got) != 0 {
		t.Errorf("dispatches = %d, want 0", len(got))
	}
}

// --- helpers ---

func gzipBytes(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		panic(err)
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func deflateBytes(b []byte) []byte {
	var buf bytes.Buffer
	zw, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		panic(err)
	}
	if _, err := zw.Write(b); err != nil {
		panic(err)
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}
