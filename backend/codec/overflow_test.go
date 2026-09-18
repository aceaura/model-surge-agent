package codec_test

import (
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
)

// 本文件守的是「输入太长」这一类错误的识别面。
//
// 漏判的后果是运维侧的：上下文超限会落到 InvalidRequest 或 Upstream，
// 后者按上游故障累计失败计数、可能冷却一个健康账号——而真正的原因是这一次
// 请求太长，换任何账号都一样。误判的后果相反：把速率限制当成请求太长，
// 本该等待重试的请求变成不换目标的 400。

func TestContextOverflowRecognizesVendorPhrasings(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  string
	}{
		// 既有的 9 条，逐条守住不回归。
		{"openai context_length", "This model's maximum context length is 8192 tokens"},
		{"code form", "context_length_exceeded"},
		{"context window", "the context window was exceeded"},
		{"maximum context", "maximum context reached"},
		{"too many tokens", "too many tokens in the request"},
		{"anthropic prompt too long", "prompt is too long: 250000 tokens > 200000 maximum"},
		{"input length", "input length exceeds the model limit"},
		{"reduce the length", "please reduce the length of the messages"},
		{"exceeds the maximum", "the request exceeds the maximum allowed"},
		// 本轮新增。
		{"anthropic request too long", "Request is too long"},
		{"gemini input token count", "The input token count exceeds the maximum number of tokens"},
		{"exceeds the context", "the prompt exceeds the context size"},
		{"context limit", "you have hit the context limit for this model"},
		// 组合式：token limit 伴随上下文语境与超出类动词。
		{"token limit with context", "context token limit exceeded for this model"},
		{"token limit too long", "the context is too long for the token limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !codec.IsContextOverflow(tc.msg) {
				t.Errorf("%q 未被识别为上下文超限，会被当成上游故障累计失败计数", tc.msg)
			}
		})
	}
}

// 不该被误判的形态。
func TestContextOverflowDoesNotOverreach(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  string
	}{
		// 裸 token limit 是速率限制的常见措辞，没有上下文语境就不算请求太长。
		// 误判会把该等待重试的 429 变成不换目标的 400。
		{"rate limit phrasing", "You have exceeded your token limit per minute"},
		{"token limit no verb", "organization token limit and context settings"},
		// max_tokens 超上限是参数问题，改小它就能过，不该被当成必须裁历史。
		// 这是本轮对 sub2api 的一处不照搬（antigravity_gateway_claude.go:502-511
		// 把裸 max_tokens 也算作 prompt 太长）。
		{"max_tokens param", "max_tokens: must be less than or equal to 8192"},
		{"unrelated", "invalid api key"},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if codec.IsContextOverflow(tc.msg) {
				t.Errorf("%q 被误判为上下文超限", tc.msg)
			}
		})
	}
}

// 大小写不敏感：各家文案首字母大小写不一致，靠调用方统一不现实。
func TestContextOverflowIsCaseInsensitive(t *testing.T) {
	for _, msg := range []string{
		"REQUEST IS TOO LONG",
		"The Input Token Count Exceeds the limit",
	} {
		if !codec.IsContextOverflow(msg) {
			t.Errorf("%q 因大小写未被识别", msg)
		}
	}
}
