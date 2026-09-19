package pipeline

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestRequest(t *testing.T) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodPost, "https://upstream.example/v1/messages", nil)
}

// 五个受保护头一个都不能写进去。逐个列出而不是遍历那个 map：
// 遍历的话有人把某一项从集合里删掉，测试会跟着少测一项而依然通过。
func TestProtectedOutboundHeadersAreDropped(t *testing.T) {
	for _, key := range []string{
		"Accept-Encoding", "Content-Length", "Transfer-Encoding", "Host", "Connection",
	} {
		t.Run(key, func(t *testing.T) {
			req := newTestRequest(t)
			notes := setOutboundHeader(req, key, "whatever", nil)
			if got := req.Header.Get(key); got != "" {
				t.Errorf("%s 被写进了出站请求：%q", key, got)
			}
			if len(notes) != 1 {
				t.Fatalf("说明条数 = %d，想要 1：丢了头却不留说明，这件事在诊断里不存在", len(notes))
			}
			if !strings.Contains(notes[0], key) {
				t.Errorf("说明没点名是哪个头：%q", notes[0])
			}
		})
	}
}

// 大小写不敏感：配置里写什么形态的都有，只拦规范形态等于没拦。
func TestProtectedHeaderMatchIsCaseInsensitive(t *testing.T) {
	for _, key := range []string{"accept-encoding", "ACCEPT-ENCODING", "Accept-Encoding"} {
		req := newTestRequest(t)
		notes := setOutboundHeader(req, key, "gzip", nil)
		if req.Header.Get("Accept-Encoding") != "" {
			t.Errorf("%q 这个形态没被拦住", key)
		}
		if len(notes) != 1 {
			t.Errorf("%q 没留说明", key)
		}
	}
}

// 说明里用规范化的头名：同一个头的各种大小写形态必须收敛成同一条说明，
// 否则 lossy 列里会出现看起来不同、其实是同一件事的多条。
func TestNoteUsesCanonicalHeaderName(t *testing.T) {
	req := newTestRequest(t)
	var notes []string
	notes = setOutboundHeader(req, "accept-encoding", "gzip", notes)
	notes = setOutboundHeader(req, "ACCEPT-ENCODING", "br", notes)
	if len(notes) != 2 {
		t.Fatalf("说明条数 = %d，想要 2", len(notes))
	}
	if notes[0] != notes[1] {
		t.Errorf("同一个头的两种形态产出了不同的说明：\n%q\n%q", notes[0], notes[1])
	}
	if !strings.Contains(notes[0], "Accept-Encoding") {
		t.Errorf("说明里不是规范形态：%q", notes[0])
	}
}

// Accept-Encoding 那条说明必须讲清后果，而不只是「被丢了」。
//
// 这一项单独测：它是五个里唯一一个写错之后请求仍然成功、
// 症状远离原因的，说明文本是运维唯一的线索。
func TestAcceptEncodingNoteExplainsTheConsequence(t *testing.T) {
	req := newTestRequest(t)
	notes := setOutboundHeader(req, "Accept-Encoding", "gzip", nil)
	if len(notes) != 1 {
		t.Fatalf("说明条数 = %d", len(notes))
	}
	if !strings.Contains(notes[0], "decompression") {
		t.Errorf("说明没讲后果，运维看不出为什么这个头不能配：%q", notes[0])
	}
}

// Content-Type 与 Accept 必须放行：上游确有要求特定取值的，
// 把它们一并拦掉会让正当的配置行为失效。
func TestContentTypeAndAcceptPassThrough(t *testing.T) {
	req := newTestRequest(t)
	var notes []string
	notes = setOutboundHeader(req, "Content-Type", "application/json; charset=utf-8", notes)
	notes = setOutboundHeader(req, "Accept", "application/json", notes)
	if got := req.Header.Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type 被拦了或写错：%q", got)
	}
	if got := req.Header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept 被拦了或写错：%q", got)
	}
	if len(notes) != 0 {
		t.Errorf("放行的头却留了说明：%v", notes)
	}
}

// 凭据头照常写进去，且说明里不带任何值。
//
// 值不进说明这条要在这里守：将来受保护集合扩大时，若某一项恰好可能带凭据，
// 「不拼值」这条规则已经在测试里钉住了。
func TestCredentialHeadersUnaffectedAndNeverInNotes(t *testing.T) {
	req := newTestRequest(t)
	var notes []string
	notes = setOutboundHeader(req, "x-api-key", "sk-secret-1234", notes)
	notes = setOutboundHeader(req, "Authorization", "Bearer sk-secret-5678", notes)
	// 同时配一个受保护头，确保 notes 非空——否则「说明里没凭据」恒成立。
	notes = setOutboundHeader(req, "Accept-Encoding", "sk-secret-in-value", notes)

	if got := req.Header.Get("x-api-key"); got != "sk-secret-1234" {
		t.Errorf("凭据头没写进去：%q", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk-secret-5678" {
		t.Errorf("Authorization 没写进去：%q", got)
	}
	if len(notes) != 1 {
		t.Fatalf("说明条数 = %d，想要 1", len(notes))
	}
	if strings.Contains(notes[0], "sk-secret") {
		t.Errorf("说明里拼上了头的值：%q", notes[0])
	}
}

// 已有的说明不能被覆盖：一次尝试里可能有多个头被丢。
func TestNotesAccumulate(t *testing.T) {
	req := newTestRequest(t)
	notes := []string{"pre-existing note"}
	notes = setOutboundHeader(req, "Host", "other.example", notes)
	notes = setOutboundHeader(req, "Connection", "close", notes)
	if len(notes) != 3 {
		t.Fatalf("说明条数 = %d，想要 3：%v", len(notes), notes)
	}
	if notes[0] != "pre-existing note" {
		t.Errorf("覆盖了调用方已有的说明：%v", notes)
	}
}
