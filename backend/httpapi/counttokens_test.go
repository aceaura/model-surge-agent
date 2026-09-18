package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// count_tokens 的回答必须是「宁多勿少」的那一侧。
//
// 客户端拿这个数字决定要不要裁上下文。低估的后果不是「数字不准」，
// 而是它裁完了还被上游以超长拒掉，而历史已经删了。

func countTokens(t *testing.T, f *fixture, body string) int64 {
	t.Helper()
	resp := f.post(t, "/v1/messages/count_tokens", body, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var got struct {
		InputTokens int64 `json:"input_tokens"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v, body = %s", err, resp.Body.String())
	}
	return got.InputTokens
}

func anthBodyWith(text string) string {
	quoted, err := json.Marshal(text)
	if err != nil {
		panic(err)
	}
	return `{"model":"demo-pool","max_tokens":64,"messages":[{"role":"user","content":` +
		string(quoted) + `}]}`
}

// 中文提示：回答不得低于「每字符 1 token」的量级。
//
// 改动前这里回 1（8 个字符除以 4 再取整），而 Anthropic 真值约 7-9，
// 中文客户端照它裁上下文会直接吃 400。
func TestCountTokensDoesNotUnderreportCJK(t *testing.T) {
	f := newFixture(t)
	const text = "你好世界你好世界" // 8 runes
	got := countTokens(t, f, anthBodyWith(text))
	if got < 8 {
		t.Errorf("input_tokens = %d，8 个中文字符至少该报 8，"+
			"低报会让客户端裁完上下文仍被上游以超长拒掉", got)
	}
}

// 英文提示仍在合理量级，不因 CJK 上调而虚高。
//
// 虚高同样有害：客户端会无谓地裁掉本来装得下的历史。
func TestCountTokensStaysReasonableForASCII(t *testing.T) {
	f := newFixture(t)
	text := strings.Repeat("hello world ", 8) // 96 ASCII chars
	got := countTokens(t, f, anthBodyWith(text))
	if got < 15 || got > 60 {
		t.Errorf("input_tokens = %d，96 个 ASCII 字符偏离每 4 字符 1 token 的量级",
			got)
	}
}

// 带图请求不得报 0：一张图在任何厂商都至少几百 token。
func TestCountTokensCountsImageBlocks(t *testing.T) {
	f := newFixture(t)
	body := `{"model":"demo-pool","max_tokens":64,"messages":[{"role":"user",
      "content":[{"type":"image","source":{"type":"base64",
      "media_type":"image/png","data":"aGVsbG8="}}]}]}`
	if got := countTokens(t, f, body); got == 0 {
		t.Error("只含一张图的请求报 0 token，带图请求会被严重低估")
	}
}

// 长中文提示的回答随长度单调增长：报一个与内容无关的常数同样满足「不低报」，
// 但那个数字毫无用处。
func TestCountTokensGrowsWithLength(t *testing.T) {
	f := newFixture(t)
	short := countTokens(t, f, anthBodyWith("你好"))
	long := countTokens(t, f, anthBodyWith(strings.Repeat("你好", 50)))
	if long <= short {
		t.Errorf("短 %d / 长 %d，回答没随内容增长", short, long)
	}
}
