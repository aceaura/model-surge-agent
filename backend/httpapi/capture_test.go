package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/capture"
	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/contract/agentv1"
	"github.com/aceaura/model-surge-agent/backend/httpapi"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
)

// withCapture 建一个开着捕获的 fixture，并把同一个 Store 挂到管理面上。
func withCapture(t *testing.T, mode capture.Mode,
	tune func(*relayclient.Target)) (*fixture, *capture.Store) {
	t.Helper()
	st := capture.New(capture.Options{Mode: mode})
	f := newFixtureTuned(t, tune, func(srv *httpapi.Server) {
		srv.Captures = st
		srv.Admin = &httpapi.Admin{Key: adminKey, Captures: st}
	})
	return f, st
}

func onlyCapture(t *testing.T, st *capture.Store) *capture.Snapshot {
	t.Helper()
	list := st.List()
	if len(list) != 1 {
		t.Fatalf("应有 1 条捕获，实际 %d 条", len(list))
	}
	return list[0]
}

// 四体齐备是这一轮的核心：任一体缺失，「转换到底破坏了什么」就有一段
// 看不见，而那一段恰好可能是坏的那一段。
func TestCaptureHoldsAllFourBodiesOnSuccess(t *testing.T) {
	f, st := withCapture(t, capture.ModeAll, nil)
	resp := f.post(t, "/v1/messages", requestBodies[codec.ProtocolAnthropic], nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
	}

	snap := onlyCapture(t, st)
	cliReq := string(snap.Bodies[capture.ClientRequest].Bytes)
	upReq := string(snap.Bodies[capture.UpstreamRequest].Bytes)
	upResp := string(snap.Bodies[capture.UpstreamResponse].Bytes)
	cliResp := string(snap.Bodies[capture.ClientResponse].Bytes)

	// 客户端请求体：客户端写的 user model。
	if !strings.Contains(cliReq, `"user-model"`) {
		t.Errorf("client_request 不是客户端发来的那份：%q", cliReq)
	}
	// 出站 wire body：已替换成上游认识的 native model。
	// 两者必须不同，否则这两体分不出是哪一步的字节。
	if !strings.Contains(upReq, `"native"`) {
		t.Errorf("upstream_request 里没有 native model，捕获点可能在替换之前：%q", upReq)
	}
	if strings.Contains(upReq, `"user-model"`) {
		t.Errorf("upstream_request 里还留着 user model：%q", upReq)
	}
	// 上游原始字节：未经解码的 SSE 帧。
	if !strings.Contains(upResp, "event: message_start") {
		t.Errorf("upstream_response 不是原始 SSE 字节：%q", upResp)
	}
	// 回客户端字节：非流式请求编成一次性 JSON。
	if !strings.Contains(cliResp, `"msg_1"`) || strings.Contains(cliResp, "event:") {
		t.Errorf("client_response 不是回给客户端的那份 JSON：%q", cliResp)
	}
}

func TestCaptureHoldsStreamingClientBytes(t *testing.T) {
	f, st := withCapture(t, capture.ModeAll, nil)
	body := `{"model":"user-model","max_tokens":64,"stream":true,
	  "messages":[{"role":"user","content":"hi"}]}`
	if resp := f.post(t, "/v1/messages", body, nil); resp.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
	}
	snap := onlyCapture(t, st)
	cliResp := string(snap.Bodies[capture.ClientResponse].Bytes)
	// 流式回的是 SSE 帧，与非流式那条路径不同，两条都要有捕获。
	if !strings.Contains(cliResp, "event: message_start") {
		t.Errorf("流式请求的 client_response 没捕到 SSE 帧：%q", cliResp)
	}
}

// 跨协议：入站 chat_completions、出站 anthropic。这正是「转换」最容易坏的
// 形态，四体必须两两不同才能定位到是哪一步坏的。
func TestCaptureCrossProtocolBodiesDiffer(t *testing.T) {
	f, st := withCapture(t, capture.ModeAll, nil)
	if resp := f.post(t, "/v1/chat/completions",
		requestBodies[codec.ProtocolChatCompletions], nil); resp.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
	}
	snap := onlyCapture(t, st)
	cliReq := string(snap.Bodies[capture.ClientRequest].Bytes)
	upReq := string(snap.Bodies[capture.UpstreamRequest].Bytes)
	cliResp := string(snap.Bodies[capture.ClientResponse].Bytes)

	// 出站是 anthropic：max_tokens 必在，而 chat_completions 的
	// content 字符串形态会被转成块数组。
	if !strings.Contains(upReq, `"max_tokens"`) {
		t.Errorf("出站 anthropic body 形状不对：%q", upReq)
	}
	if cliReq == upReq {
		t.Error("入站与出站 body 完全相同：跨协议转换必然改形，两者相同说明捕获点重了")
	}
	// 回客户端的是 chat_completions 外形。
	if !strings.Contains(cliResp, `"chat.completion"`) {
		t.Errorf("client_response 不是 chat_completions 外形：%q", cliResp)
	}
}

// 上游忽略 stream:true 回整份 JSON 是常见形态。这条分支单独走
// adoptWholeResponse，漏掉它会让「非 SSE 上游」这一类问题恰好没有字节可看。
func TestCaptureCoversNonSSEUpstream(t *testing.T) {
	whole := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_whole","type":"message","role":"assistant",` +
			`"model":"native","content":[{"type":"text","text":"hi"}],` +
			`"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`))
	}))
	t.Cleanup(whole.Close)

	f, st := withCapture(t, capture.ModeAll, func(tg *relayclient.Target) {
		tg.BaseURL = whole.URL
	})
	if resp := f.post(t, "/v1/messages",
		requestBodies[codec.ProtocolAnthropic], nil); resp.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
	}
	snap := onlyCapture(t, st)
	if got := string(snap.Bodies[capture.UpstreamResponse].Bytes); !strings.Contains(got, "msg_whole") {
		t.Errorf("非 SSE 上游的原始字节没被捕获：%q", got)
	}
}

// 凭据绝不入捕获。捕获只存 body，而上游凭据只存在于请求头里——
// 这条断言钉住的是「永不捕获请求头」这个设计，不是某个脱敏函数。
func TestCaptureNeverContainsUpstreamCredentials(t *testing.T) {
	const secret = "sk-upstream-super-secret"
	f, st := withCapture(t, capture.ModeAll, func(tg *relayclient.Target) {
		tg.Headers = map[string]string{
			"x-api-key":     secret,
			"authorization": "Bearer " + secret,
		}
	})
	if resp := f.post(t, "/v1/messages", requestBodies[codec.ProtocolAnthropic],
		map[string]string{"Authorization": "Bearer sk-client-secret"}); resp.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
	}
	snap := onlyCapture(t, st)
	for i := range snap.Bodies {
		blob := string(snap.Bodies[i].Bytes)
		for _, bad := range []string{secret, "sk-client-secret"} {
			if strings.Contains(blob, bad) {
				t.Errorf("体 %d 里出现了凭据：捕获只能存 body，永不存请求头。%q", i, blob)
			}
		}
	}
}

// errors 档只留失败的请求。这是默认建议档：常开而不撑爆内存。
func TestErrorsModeKeepsOnlyFailedRequestsEndToEnd(t *testing.T) {
	f, st := withCapture(t, capture.ModeErrors, nil)
	if resp := f.post(t, "/v1/messages",
		requestBodies[codec.ProtocolAnthropic], nil); resp.Code != http.StatusOK {
		t.Fatalf("成功请求 status = %d", resp.Code)
	}
	if got := len(st.List()); got != 0 {
		t.Fatalf("errors 档留下了成功请求的捕获：%d 条", got)
	}

	// 模型不存在：受理通过、dispatch 失败，走 fail 路径。
	resp := f.post(t, "/v1/messages",
		`{"model":"user-model","max_tokens":64,"messages":[]}`, nil)
	if resp.Code == http.StatusOK {
		t.Fatalf("这条本该失败：%s", resp.Body.String())
	}
	snap := onlyCapture(t, st)
	if len(snap.Bodies[capture.ClientRequest].Bytes) == 0 {
		t.Error("失败请求的 client_request 是空的")
	}
	// 错误信封也是回客户端的字节，而 errors 档唯一留下的就是这类请求，
	// 漏掉它会让被保留的捕获恰好缺第四体。
	if got := string(snap.Bodies[capture.ClientResponse].Bytes); !strings.Contains(got, `"error"`) {
		t.Errorf("失败请求的 client_response 没捕到错误信封：%q", got)
	}
}

func TestOffModeCapturesNothingEndToEnd(t *testing.T) {
	f, st := withCapture(t, capture.ModeOff, nil)
	if resp := f.post(t, "/v1/messages",
		requestBodies[codec.ProtocolAnthropic], nil); resp.Code != http.StatusOK {
		t.Fatalf("status = %d", resp.Code)
	}
	f.post(t, "/v1/messages", `{"model":"user-model","max_tokens":64,"messages":[]}`, nil)
	if got := len(st.List()); got != 0 {
		t.Errorf("off 档留下了 %d 条捕获", got)
	}
}

// 捕获关闭时（Server.Captures 为 nil）数据面必须照常工作：
// nil 接收者的每个方法都是空操作，调用点不需要判空。
func TestNilCaptureStoreDoesNotBreakDataPlane(t *testing.T) {
	f := newFixture(t)
	if resp := f.post(t, "/v1/messages",
		requestBodies[codec.ProtocolAnthropic], nil); resp.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
	}
}

func TestCaptureEndpointsRequireAdminKey(t *testing.T) {
	f, st := withCapture(t, capture.ModeAll, nil)
	f.post(t, "/v1/messages", requestBodies[codec.ProtocolAnthropic], nil)
	id := onlyCapture(t, st).RequestID

	for _, path := range []string{"/admin/captures", "/admin/captures/" + id} {
		for _, key := range []string{"", "wrong-key"} {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			if key != "" {
				r.Header.Set("Authorization", "Bearer "+key)
			}
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("%s key=%q: status = %d, want 401 —— "+
					"捕获里是请求全文，鉴权失效等于把它公开", path, key, w.Code)
			}
		}
	}
}

func (f *fixture) adminGet(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("Authorization", "Bearer "+adminKey)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

func TestCaptureListGivesSizesNotBytes(t *testing.T) {
	f, _ := withCapture(t, capture.ModeAll, nil)
	f.post(t, "/v1/messages", requestBodies[codec.ProtocolAnthropic], nil)

	resp := f.adminGet(t, "/admin/captures")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
	}
	var list agentv1.CaptureList
	if err := json.Unmarshal(resp.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if list.Mode != string(capture.ModeAll) {
		t.Errorf("mode = %q, want all —— 列表为空时要能区分「关着」与「没有符合条件的请求」", list.Mode)
	}
	if len(list.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(list.Items))
	}
	for _, key := range []string{"client_request", "upstream_request",
		"upstream_response", "client_response"} {
		if list.Items[0].Sizes[key] <= 0 {
			t.Errorf("sizes[%q] = %d，四体都该有字节", key, list.Items[0].Sizes[key])
		}
	}
	// 列表不能带字节：一条捕获可达数 MB，列 32 条就是上百 MB 的响应。
	if strings.Contains(resp.Body.String(), "message_start") {
		t.Errorf("列表里带上了 body 字节：%s", resp.Body.String())
	}
}

func TestCaptureDetailReturnsPlainBodiesNotBase64(t *testing.T) {
	f, st := withCapture(t, capture.ModeAll, nil)
	f.post(t, "/v1/messages", requestBodies[codec.ProtocolAnthropic], nil)
	id := onlyCapture(t, st).RequestID

	resp := f.adminGet(t, "/admin/captures/"+id)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
	}
	var detail agentv1.CaptureDetail
	if err := json.Unmarshal(resp.Body.Bytes(), &detail); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if detail.RequestID != id {
		t.Errorf("request_id = %q, want %q", detail.RequestID, id)
	}
	// 明文而非 base64：这个端点唯一的用途是人眼看哪一步坏了，
	// base64 之后要先解一层才能看。
	if !strings.Contains(detail.UpstreamResponse.Body, "event: message_start") {
		t.Errorf("upstream_response 不是明文：%q", detail.UpstreamResponse.Body)
	}
	if !strings.Contains(detail.UpstreamRequest.Body, `"native"`) {
		t.Errorf("upstream_request 不是明文：%q", detail.UpstreamRequest.Body)
	}
	if detail.ClientRequest.Truncated || detail.ClientRequest.Dropped != 0 {
		t.Errorf("小请求被标成截断：%+v", detail.ClientRequest)
	}
}

// 回 404 而不是空对象：捕获是有上限的环，那一条可能已被淘汰，
// 空对象会让读的人以为那次请求四体全空。
func TestCaptureDetailIs404WhenEvictedOrUnknown(t *testing.T) {
	f, _ := withCapture(t, capture.ModeAll, nil)
	resp := f.adminGet(t, "/admin/captures/no-such-request")
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", resp.Code, resp.Body.String())
	}
	var env agentv1.ErrorEnvelope
	if err := json.Unmarshal(resp.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Error.Code != agentv1.CodeNotFound {
		t.Errorf("code = %q, want not_found", env.Error.Code)
	}
}

// 捕获未装配时管理面回空列表而不是崩：nil *Store 的方法全是空操作。
func TestCaptureEndpointsSurviveNilStore(t *testing.T) {
	a := newAdmin(t)
	resp := a.get(t, "/admin/captures", adminKey)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
	}
	var list agentv1.CaptureList
	if err := json.Unmarshal(resp.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if list.Mode != string(capture.ModeOff) || len(list.Items) != 0 {
		t.Errorf("未装配时应回 off 与空列表，实际 %+v", list)
	}
	if got := a.get(t, "/admin/captures/x", adminKey).Code; got != http.StatusNotFound {
		t.Errorf("未装配时详情应回 404，实际 %d", got)
	}
}

// 捕获的请求 ID 必须与响应头回显的一致，否则运维拿着头里的 ID 查不到捕获。
func TestCaptureIDMatchesResponseHeader(t *testing.T) {
	f, st := withCapture(t, capture.ModeAll, nil)
	resp := f.post(t, "/v1/messages", requestBodies[codec.ProtocolAnthropic], nil)
	want := resp.Header().Get("X-Request-Id")
	if want == "" {
		t.Fatal("响应没回显 request id")
	}
	if got := onlyCapture(t, st).RequestID; got != want {
		t.Errorf("捕获 ID = %q，响应头 = %q，两者必须一致才查得到", got, want)
	}
}

// 解码就失败的请求不开捕获：它连出站协议都没选过，四体里只会有一体。
func TestMalformedRequestIsNotCaptured(t *testing.T) {
	f, st := withCapture(t, capture.ModeAll, nil)
	if resp := f.post(t, "/v1/messages", `{not json`, nil); resp.Code == http.StatusOK {
		t.Fatal("畸形 JSON 竟然成功了")
	}
	if got := len(st.List()); got != 0 {
		t.Errorf("受理面拒绝的请求留下了 %d 条捕获，那里面只会有一体，是噪音", got)
	}
}
