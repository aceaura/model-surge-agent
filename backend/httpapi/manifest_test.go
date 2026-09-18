package httpapi_test

import (
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
)

// 这一组测的是「清单与单模型查询的外形对不对、按什么定」。
// 模型清单本身的内容来自调度层，那部分属于 relayclient，不在这里重复。

// familyMarker 是各族清单独有的字段，用来判断回的是哪一族的外形。
var familyMarker = map[string]string{
	"anthropic":        `"display_name"`,
	"chat_completions": `"owned_by"`,
	"gemini":           `"supportedGenerationMethods"`,
}

// assertFamily 断言响应体是某一族的外形，且不带别族的标记。
func assertFamily(t *testing.T, body, family string) {
	t.Helper()
	if !strings.Contains(body, familyMarker[family]) {
		t.Errorf("want %s shape (%s), got %s", family, familyMarker[family], body)
	}
	for other, marker := range familyMarker {
		if other == family {
			continue
		}
		if strings.Contains(body, marker) {
			t.Errorf("%s shape must not carry %s: %s", family, marker, body)
		}
	}
}

// TestSharedManifestPathFollowsClientSignals 是共享路径上的头→族推断矩阵。
//
// /v1/models 与 /models 两族 SDK 都会打：用户把 base_url 配成 host，
// Anthropic SDK 与 OpenAI SDK 各自拼出同一个路径。给错了族对方解析不出来。
func TestSharedManifestPathFollowsClientSignals(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"anthropic-version", map[string]string{"anthropic-version": "2023-06-01"}, "anthropic"},
		{"x-api-key", map[string]string{"x-api-key": "sk-1"}, "anthropic"},
		{"x-goog-api-key", map[string]string{"x-goog-api-key": "k"}, "gemini"},
		{"bearer", map[string]string{"Authorization": "Bearer sk-1"}, "chat_completions"},
		// 什么专有信号都没有时落到 openai：它是唯一没有专有头的一族，
		// 只能当兜底，而这也是最常见的客户端形态。
		{"no-signal", nil, "chat_completions"},
	}
	for _, path := range []string{"/v1/models", "/v1/v1/models", "/models"} {
		for _, c := range cases {
			t.Run(path+"/"+c.name, func(t *testing.T) {
				f := newFixture(t)
				resp := f.getWith(t, path, c.headers)
				if resp.Code != http.StatusOK {
					t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
				}
				assertFamily(t, resp.Body.String(), c.want)
			})
		}
	}
}

// TestGeminiQueryKeyAlsoSelectsGemini 覆盖 Gemini 的另一种凭据位置。
//
// Gemini 客户端可以把 key 放 query 而不是头，两种都得认：只认头会让一半
// Gemini 客户端拿到 openai 外形。
func TestGeminiQueryKeyAlsoSelectsGemini(t *testing.T) {
	f := newFixture(t)
	resp := f.get(t, "/v1/models?key=abc")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
	}
	assertFamily(t, resp.Body.String(), "gemini")
}

// TestExplicitPathBeatsHeaders 钉住路径优先于头。
//
// /anthropic/... 与 /openai/... 是用户显式选的族，头只是推断。让头翻盘会
// 让「我明明配了 /openai 前缀」这件事失效，而用户无从判断为什么。
func TestExplicitPathBeatsHeaders(t *testing.T) {
	cases := []struct {
		path    string
		headers map[string]string
		want    string
	}{
		{"/anthropic/v1/models", map[string]string{"Authorization": "Bearer sk-1"}, "anthropic"},
		{"/openai/v1/models", map[string]string{"anthropic-version": "2023-06-01"}, "chat_completions"},
		{"/openai/v1/models", map[string]string{"x-goog-api-key": "k"}, "chat_completions"},
		{"/v1beta/models", map[string]string{"x-api-key": "sk-1"}, "gemini"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			f := newFixture(t)
			resp := f.getWith(t, c.path, c.headers)
			if resp.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
			}
			assertFamily(t, resp.Body.String(), c.want)
		})
	}
}

// TestHeaderPrecedenceIsAnthropicThenGemini 钉住多族头同时出现时的次序。
//
// 次序不是随意的：前两族要有专有头才成立，openai 是「什么都没有」那一档。
// 同时带 anthropic 与 gemini 头只会出现在代理链上，此时取 anthropic——
// 它的头更专一（anthropic-version 没有别的含义），gemini 的 key 参数则
// 常被中间层顺手加上。
func TestHeaderPrecedenceIsAnthropicThenGemini(t *testing.T) {
	f := newFixture(t)
	resp := f.getWith(t, "/v1/models", map[string]string{
		"anthropic-version": "2023-06-01",
		"x-goog-api-key":    "k",
		"Authorization":     "Bearer sk-1",
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
	}
	assertFamily(t, resp.Body.String(), "anthropic")
}

// TestGeminiManifestUsesResourceNames 钉住 Gemini 清单的资源名形态。
//
// Gemini 的清单元素用 name: "models/xxx"，客户端会把这个串原样回传去做
// 单模型查询。裸 id 会让它拼出 models/models/xxx。
func TestGeminiManifestUsesResourceNames(t *testing.T) {
	f := newFixture(t)
	resp := f.get(t, "/v1beta/models")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
	}
	var out struct {
		Models []struct {
			Name                       string   `json:"name"`
			DisplayName                string   `json:"displayName"`
			SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Models) != 1 {
		t.Fatalf("models = %d, want 1 (disabled ones excluded)", len(out.Models))
	}
	m := out.Models[0]
	if m.Name != "models/user-model" {
		t.Errorf("name = %q, want models/user-model", m.Name)
	}
	if m.DisplayName != "user-model" {
		t.Errorf("displayName = %q, want user-model", m.DisplayName)
	}
	if len(m.SupportedGenerationMethods) == 0 {
		t.Error("supportedGenerationMethods must be listed: 客户端据此判断能不能流式")
	}
}

// TestSingleModelLookupWorksOnEveryManifestPath 是单模型查询的全路径覆盖。
//
// SDK 的 models.retrieve() 是在它拿清单的那个 base_url 上拼 /{id}，
// 所以每条清单路径都得有配对的单查端点，少一条就有一类客户端 404。
func TestSingleModelLookupWorksOnEveryManifestPath(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/v1/models/user-model", "chat_completions"},
		{"/v1/v1/models/user-model", "chat_completions"},
		{"/models/user-model", "chat_completions"},
		{"/anthropic/v1/models/user-model", "anthropic"},
		{"/openai/v1/models/user-model", "chat_completions"},
		{"/v1beta/models/user-model", "gemini"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			f := newFixture(t)
			resp := f.get(t, c.path)
			if resp.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
			}
			body := resp.Body.String()
			assertFamily(t, body, c.want)
			// 单查回的是一个模型对象，不是清单包装。
			if strings.Contains(body, `"data"`) || strings.Contains(body, `"models"`) {
				t.Errorf("single lookup must not return a list wrapper: %s", body)
			}
			if !strings.Contains(body, "user-model") {
				t.Errorf("body missing the model id: %s", body)
			}
		})
	}
}

// TestSingleModelLookupFollowsClientSignals 单查的外形与清单同一套推断。
//
// 两者必须一致：客户端先拿清单再单查，中途换族会让它解析不出来。
func TestSingleModelLookupFollowsClientSignals(t *testing.T) {
	f := newFixture(t)
	resp := f.getWith(t, "/v1/models/user-model",
		map[string]string{"anthropic-version": "2023-06-01"})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
	}
	assertFamily(t, resp.Body.String(), "anthropic")
}

// TestGeminiResourceNameRoundTripsThroughLookup 钉住带斜杠的 id 能查到。
//
// Gemini 客户端回传清单里的 models/xxx，那里面有斜杠。单段路径模式匹配
// 不到它，查找键也得把前缀剥掉，两处都错一个就 404。
func TestGeminiResourceNameRoundTripsThroughLookup(t *testing.T) {
	f := newFixture(t)
	resp := f.get(t, "/v1beta/models/models/user-model")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
	}
	assertFamily(t, resp.Body.String(), "gemini")
	if !strings.Contains(resp.Body.String(), `"models/user-model"`) {
		t.Errorf("want the resource name echoed back: %s", resp.Body.String())
	}
}

// TestUnknownModelLookupIsANotFoundEnvelope 钉住缺失模型回 404 信封。
//
// 不能回 200 包一个错误体（new-api 的 controller/model.go:369 就是这么做的）：
// SDK 看到 200 会按成功去解析，拿到一个缺字段的对象，报错指向的是字段名而
// 不是「这个模型不存在」。
func TestUnknownModelLookupIsANotFoundEnvelope(t *testing.T) {
	f := newFixture(t)
	resp := f.getWith(t, "/v1/models/nope", map[string]string{"anthropic-version": "2023-06-01"})
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", resp.Code, resp.Body.String())
	}
	var out struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not an anthropic error envelope: %v: %s", err, resp.Body.String())
	}
	if out.Type != "error" || out.Error.Type != "not_found_error" {
		t.Errorf("envelope = %+v, want an anthropic not_found_error", out)
	}
	if !strings.Contains(out.Error.Message, "nope") {
		t.Errorf("message must name the model asked for: %q", out.Error.Message)
	}
}

// TestDisabledModelLookupIsNotFound 钉住禁用的模型按不存在处理。
//
// 清单不列它，单查却回它会让客户端拿到一个必然失败的 id——而它从清单里
// 根本看不到这个 id，无从判断为什么失败。
func TestDisabledModelLookupIsNotFound(t *testing.T) {
	f := newFixture(t)
	resp := f.get(t, "/v1/models/disabled-model")
	if resp.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: %s", resp.Code, resp.Body.String())
	}
}

// TestModelLookupDoesNotCallTheUpstream 钉住单查不打上游。
//
// 查清单是本地代理调度层的数据，打上游会白耗一次配额。
func TestModelLookupDoesNotCallTheUpstream(t *testing.T) {
	f := newFixture(t)
	f.get(t, "/v1/models/user-model")
	f.get(t, "/v1/models")
	if f.upstreamCalls() != 0 {
		t.Errorf("upstream calls = %d, want 0", f.upstreamCalls())
	}
	if len(f.relay.Dispatches()) != 0 {
		t.Error("manifest lookups must not dispatch")
	}
}

// TestEveryManifestPathHasASingleModelSibling 是源码级守卫。
//
// 光靠上面那张表不够：新增一条清单路径时，表不会自己长出一行，而漏注册
// 单查端点的后果是某一类客户端的 retrieve() 静默 404。这里直接盯注册代码。
func TestEveryManifestPathHasASingleModelSibling(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	text := string(src)
	if !strings.Contains(text, `mux.HandleFunc(http.MethodGet+" "+path, s.listModels(family))`) {
		t.Error("清单注册那行变了，本守卫失效，请跟着改")
	}
	if !strings.Contains(text, `mux.HandleFunc(http.MethodGet+" "+path+modelIDSuffix, s.getModel(family))`) {
		t.Error("单模型查询必须与清单在同一个循环里成对注册")
	}

	routes, err := os.ReadFile("routes.go")
	if err != nil {
		t.Fatalf("read routes.go: %v", err)
	}
	// 通配必须是 {id...}：单段的 {id} 匹配不到 Gemini 回传的 models/xxx。
	if !regexp.MustCompile(`modelIDSuffix\s*=\s*"/\{id\.\.\.\}"`).Match(routes) {
		t.Error("modelIDSuffix 必须用 {id...} 通配，单段模式会漏掉带斜杠的资源名")
	}
}
