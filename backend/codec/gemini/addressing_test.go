package gemini

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// 本文件守出站寻址：模型名进路径这件事只有本协议有，所以「模型名里的字符
// 改变了 URL 结构」这个缺口也只有本协议有。判据 10–19。

// parse 把拼出来的 URL 交给标准库解析，断言的是「请求真正会打到哪」，
// 而不是字符串长什么样——中间那层解析正是缺口所在。
func parse(t *testing.T, raw string) *url.URL {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, raw, nil)
	if err != nil {
		t.Fatalf("NewRequest(%q): %v", raw, err)
	}
	return req.URL
}

// 判据 10：正常模型名逐字节不变，查询串在。
func TestNormalModelNameIsUnchanged(t *testing.T) {
	raw, _ := outboundCodec{}.Endpoint("https://host/v1beta", "gemini-3-pro", true)
	u := parse(t, raw)
	if u.Path != "/v1beta/models/gemini-3-pro:streamGenerateContent" {
		t.Errorf("path = %q", u.Path)
	}
	if u.RawQuery != "alt=sse" {
		t.Errorf("query = %q", u.RawQuery)
	}
}

// 判据 11：问号被转义，不再把方法名与 alt=sse 推进查询串。
func TestQuestionMarkInModelNameStaysInThePath(t *testing.T) {
	raw, _ := outboundCodec{}.Endpoint("https://host/v1beta", "gemini?x", true)
	u := parse(t, raw)
	if !strings.HasSuffix(u.Path, ":streamGenerateContent") {
		t.Errorf("方法名被挤出了路径：path = %q", u.Path)
	}
	if u.RawQuery != "alt=sse" {
		t.Errorf("query = %q，want alt=sse——丢了它上游回整份 JSON 而不是 SSE", u.RawQuery)
	}
}

// 判据 12：井号最危险——不转义时整个查询串消失，alt=sse 一起没了。
//
// 后果不是一次 404 而是一次「HTTP 200 但一帧都解不出来」：那条路是可重试的，
// 于是三个同名目标连挂三次，而运维看到的是三个账号同时坏掉。
func TestFragmentMarkerInModelNameDoesNotDropTheQuery(t *testing.T) {
	raw, _ := outboundCodec{}.Endpoint("https://host/v1beta", "gemini#x", true)
	u := parse(t, raw)
	if u.RawQuery != "alt=sse" {
		t.Errorf("query = %q，want alt=sse", u.RawQuery)
	}
	if u.Fragment != "" {
		t.Errorf("fragment = %q，模型名里的 # 被当成了片段分隔符", u.Fragment)
	}
	if !strings.HasSuffix(u.Path, ":streamGenerateContent") {
		t.Errorf("path = %q", u.Path)
	}
}

// 判据 13：空格转义成 %20，不靠标准库的宽容。
func TestSpaceInModelNameIsEscaped(t *testing.T) {
	raw, _ := outboundCodec{}.Endpoint("https://host/v1beta", "gemini x", true)
	if strings.Contains(raw, "gemini x") {
		t.Errorf("空格未转义：%q", raw)
	}
	u := parse(t, raw)
	if u.Path != "/v1beta/models/gemini x:streamGenerateContent" {
		t.Errorf("path = %q（解码后应还原成原名）", u.Path)
	}
	if u.RawQuery != "alt=sse" {
		t.Errorf("query = %q", u.RawQuery)
	}
}

// 判据 14：带路径分隔符的模型名一律拒绝，而不是转义。
//
// PathEscape 不编码 . 与 /，`../v1beta/models/other` 转义后照样是一条
// 能走到别的端点上去的相对路径。
func TestPathTraversalModelNameIsRejected(t *testing.T) {
	for _, name := range []string{
		"../v1beta/models/other",
		"a/b",
		"/leading",
		"trailing/",
		`a\b`,
		"..",
		".",
		"",
	} {
		raw, _ := outboundCodec{}.Endpoint("https://host/v1beta", name, true)
		if raw != "" {
			t.Errorf("Endpoint(%q) = %q，want 空串（拒绝）", name, raw)
		}
		rawNS, _ := outboundCodec{}.Endpoint("https://host/v1beta", name, false)
		if rawNS != "" {
			t.Errorf("非流式 Endpoint(%q) = %q，want 空串", name, rawNS)
		}
	}
}

// 判据 15：单个点号只在整段就是它时才是穿越；`gemini-2.5-pro` 必须照常放行。
func TestDotsInsideModelNameAreFine(t *testing.T) {
	raw, _ := outboundCodec{}.Endpoint("https://host/v1beta", "gemini-2.5-pro", true)
	u := parse(t, raw)
	if u.Path != "/v1beta/models/gemini-2.5-pro:streamGenerateContent" {
		t.Errorf("path = %q——版本号里的点被当成穿越拒掉了", u.Path)
	}
}

// 判据 16：base_url 已经带 /models 时不拼出 /models/models。
func TestBaseURLEndingInModelsIsNotDoubled(t *testing.T) {
	for _, base := range []string{
		"https://host/v1beta/models",
		"https://host/v1beta/models/",
	} {
		raw, _ := outboundCodec{}.Endpoint(base, "gemini-3-pro", true)
		u := parse(t, raw)
		if u.Path != "/v1beta/models/gemini-3-pro:streamGenerateContent" {
			t.Errorf("base %q → path %q", base, u.Path)
		}
	}
}

// 判据 17：只削一层。配成 .../models/models 的人要的就是那个路径。
func TestBaseURLTrimsOnlyOneModelsSuffix(t *testing.T) {
	raw, _ := outboundCodec{}.Endpoint("https://host/v1beta/models/models", "gemini-3-pro", true)
	u := parse(t, raw)
	if u.Path != "/v1beta/models/models/gemini-3-pro:streamGenerateContent" {
		t.Errorf("path = %q——循环削到没有会改掉一个可能合法的上游路径", u.Path)
	}
}

// 判据 18：非流式路径同样走转义与拒绝，不能只修流式那一条。
func TestNonStreamPathIsEscapedToo(t *testing.T) {
	raw, _ := outboundCodec{}.Endpoint("https://host/v1beta", "gemini#x", false)
	u := parse(t, raw)
	if !strings.HasSuffix(u.Path, ":generateContent") {
		t.Errorf("path = %q", u.Path)
	}
	if u.RawQuery != "" {
		t.Errorf("非流式不该有查询串，得到 %q", u.RawQuery)
	}
}

// 判据 19：Endpoint 不返回额外请求头（本协议的 key 走 target.headers）。
func TestEndpointReturnsNoExtraHeaders(t *testing.T) {
	_, extra := outboundCodec{}.Endpoint("https://host/v1beta", "gemini-3-pro", true)
	if len(extra) != 0 {
		t.Errorf("extra = %v", extra)
	}
}
