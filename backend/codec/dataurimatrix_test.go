package codec_test

import (
	"encoding/json"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// dataURICases 是 data URI 解析的判据表，两个协议共用。
//
// 两个协议各留一份解析代码（合并要把「用单个字符串同时表达内联与远程」这个
// wire 层细节上提到中立层），所以一致性只能靠这张共用的表顶住：改一份不改另一份，
// 下面那条跨协议测试会红。
var dataURICases = []struct {
	name string
	url  string
	// wantInline 为真时期望解成内联载荷，为假时期望整段留在 URL 位。
	wantInline bool
	wantMedia  string
	wantData   string
}{
	{
		name: "只有 base64 参数", url: "data:image/png;base64,QUJD",
		wantInline: true, wantMedia: "image/png", wantData: "QUJD",
	},
	{
		// 这一格是本轮修的那条：只切第一个分号时 encoding 拿到
		// "charset=utf-8;base64"，比不上 "base64"，于是一份合法的内联图片
		// 被当成远程链接，上游去拉一个几百 KB 的伪 URL。
		name: "charset 参数在前", url: "data:image/png;charset=utf-8;base64,QUJD",
		wantInline: true, wantMedia: "image/png", wantData: "QUJD",
	},
	{
		name: "多个参数", url: "data:application/pdf;charset=utf-8;foo=bar;base64,QUJD",
		wantInline: true, wantMedia: "application/pdf", wantData: "QUJD",
	},
	{
		// RFC 2397 没规定大小写。上游与 SDK 都见过大写写法。
		name: "BASE64 大写", url: "data:image/png;charset=utf-8;BASE64,QUJD",
		wantInline: true, wantMedia: "image/png", wantData: "QUJD",
	},
	{
		// media type 为空是合法的（RFC 默认 text/plain），交给 SniffMediaType 去猜。
		name: "media type 缺省", url: "data:;base64,QUJD",
		wantInline: true, wantMedia: "", wantData: "QUJD",
	},
	{
		// 纯文本 data URI：载荷不是 base64。认下来会让出站编出一份上游解不开的
		// 载荷——Media.Data 这一位的语义就是 base64。
		name: "无 base64 参数", url: "data:text/plain,hello", wantInline: false,
	},
	{
		// 有参数列表但末位不是 base64：载荷是 URL 编码的文本而非 base64。
		// 上一行那格没有分号、走的是「压根没有参数」那条分支，测不到
		// 「末位参数不认」这一格——少了这格，「有参数就认成 base64」也能全绿。
		name: "有参数但末位不是 base64", url: "data:text/plain;charset=utf-8,hello", wantInline: false,
	},
	{
		// 这里的 base64 占的是 media type 位，不是参数位。
		name: "base64 当成了 media type", url: "data:base64,QUJD", wantInline: false,
	},
	{
		name: "没有逗号", url: "data:image/png;base64", wantInline: false,
	},
	{
		name: "远程链接", url: "https://example.com/a.png", wantInline: false,
	},
}

// chatCompletionsImageBody 把一个 image_url 包成本协议的最小请求体。
func chatCompletionsImageBody(t *testing.T, url string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model": "m", "max_tokens": 16,
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type": "image_url", "image_url": map[string]any{"url": url},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("编请求体：%v", err)
	}
	return body
}

// responsesImageBody 把同一个 URL 包成 responses 协议的最小请求体。
func responsesImageBody(t *testing.T, url string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model": "m", "max_output_tokens": 16,
		"input": []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_image", "image_url": url,
			}},
		}},
	})
	if err != nil {
		t.Fatalf("编请求体：%v", err)
	}
	return body
}

func onlyImageMedia(t *testing.T, req *ir.Request) *ir.Media {
	t.Helper()
	if len(req.Messages) != 1 {
		t.Fatalf("消息数 = %d，want 1", len(req.Messages))
	}
	for _, b := range req.Messages[0].Content {
		if b.Type == ir.BlockImage {
			if b.Media == nil {
				t.Fatal("图片块没有载荷")
			}
			return b.Media
		}
	}
	t.Fatalf("没有图片块：%+v", req.Messages[0].Content)
	return nil
}

// 判据 7/8：两个协议对同一组 data URI 的解析结果必须一致。
//
// 逐格断言期望值，同时断言两侧相同。只断言「两侧相同」不够——两份同时改错也
// 相同；只逐格断言期望值也不够——那样改一份漏一份时只有一个协议红，而红的那个
// 可能被误认为是夹具问题。两条一起才能指出「另一份没跟改」。
func TestDataURIParsedTheSameByBothProtocols(t *testing.T) {
	cc, ok := codec.Inbound(codec.ProtocolChatCompletions)
	if !ok {
		t.Fatal("chat_completions 入站没注册")
	}
	rs, ok := codec.Inbound(codec.ProtocolResponses)
	if !ok {
		t.Fatal("responses 入站没注册")
	}

	for _, c := range dataURICases {
		t.Run(c.name, func(t *testing.T) {
			ccReq, err := cc.DecodeRequest(chatCompletionsImageBody(t, c.url))
			if err != nil {
				t.Fatalf("chat_completions 解码失败：%v", err)
			}
			rsReq, err := rs.DecodeRequest(responsesImageBody(t, c.url))
			if err != nil {
				t.Fatalf("responses 解码失败：%v", err)
			}
			ccMedia := onlyImageMedia(t, ccReq)
			rsMedia := onlyImageMedia(t, rsReq)

			for name, got := range map[string]*ir.Media{"chat_completions": ccMedia, "responses": rsMedia} {
				if c.wantInline {
					if got.Data != c.wantData {
						t.Errorf("%s: Data = %q，want %q", name, got.Data, c.wantData)
					}
					if got.MediaType != c.wantMedia {
						t.Errorf("%s: MediaType = %q，want %q", name, got.MediaType, c.wantMedia)
					}
					if got.URL != "" {
						t.Errorf("%s: 内联载荷落到了 URL 位：%q——上游会去拉这个伪链接", name, got.URL)
					}
					continue
				}
				if got.URL != c.url {
					t.Errorf("%s: URL = %q，want 原样保留 %q", name, got.URL, c.url)
				}
				if got.Data != "" {
					t.Errorf("%s: 非 base64 载荷被当成内联数据：%q——出站会编出一份"+
						"上游解不开的载荷", name, got.Data)
				}
			}

			if ccMedia.Data != rsMedia.Data || ccMedia.MediaType != rsMedia.MediaType ||
				ccMedia.URL != rsMedia.URL {
				t.Errorf("两个协议解出的结果不同：chat_completions=%+v responses=%+v；"+
					"两份解析代码有一份没跟改", ccMedia, rsMedia)
			}
		})
	}
}
