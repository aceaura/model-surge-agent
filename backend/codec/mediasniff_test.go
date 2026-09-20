package codec

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// pngBytes 是一份足够长的 PNG 头，保证探测窗口里有 12 个有效字节。
func pngBytes() []byte {
	return []byte("\x89PNG\r\n\x1a\n" + "0123456789abcdef")
}

// 判据 1：折行的 base64 仍能判出类型。
//
// 很多 SDK 按 76 列折行，此前 StdEncoding 遇到 \n 直接报错、返回空串，
// 而那个空串让 AcceptsMedia 恒假、媒体被整块降级成文本备注——客户端上传的是
// 一份完全可解码的图片，模型却什么都没看到。
func TestFoldedBase64IsStillSniffed(t *testing.T) {
	std := base64.StdEncoding.EncodeToString(pngBytes())
	for name, data := range map[string]string{
		"lf":      std[:8] + "\n" + std[8:],
		"crlf":    std[:8] + "\r\n" + std[8:],
		"space":   std[:8] + " " + std[8:],
		"tab":     std[:8] + "\t" + std[8:],
		"leading": "\n" + std,
		// 每 4 个字符一断，把整个探测窗口打散：剥空白必须边扫边剥，
		// 先截 16 个字符再剥的话这里只剩 4 个有效字符，解出 3 字节判不出 PNG。
		"dense": strings.Join([]string{std[:4], std[4:8], std[8:12], std[12:16], std[16:]}, "\n"),
	} {
		if got := SniffMediaType(&ir.Media{Data: data}); got != "image/png" {
			t.Errorf("%s: 嗅探 = %q，want image/png；空串会让这份媒体被降级成文本", name, got)
		}
	}
}

// 判据 2：URL-safe 字母表也要认。
//
// 标准库的两个字母表是独立编码器，只试 StdEncoding 时含 - 或 _ 的载荷永远
// 判不出类型。用一份必然含这两个字符的载荷，否则两种字母表编出同样的字符串，
// 去掉回落分支这条测试照样绿。
func TestURLSafeBase64IsStillSniffed(t *testing.T) {
	// 0xff 0xd8 0xff 是 JPEG 魔数；后面那几个字节挑得让编码结果含 - 与 _。
	raw := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0xfb, 0xff, 0xfe, 0xfd, 0x3f, 0x3e}
	urlsafe := base64.URLEncoding.EncodeToString(raw)
	if !strings.ContainsAny(urlsafe, "-_") {
		t.Fatalf("夹具失效：%q 不含 URL-safe 专有字符，两种字母表编出同一串，"+
			"去掉回落分支也测不出来", urlsafe)
	}
	if got := SniffMediaType(&ir.Media{Data: urlsafe}); got != "image/jpeg" {
		t.Errorf("URL-safe 载荷嗅探 = %q，want image/jpeg", got)
	}
	// 同一份字节用标准字母表编出来当然也要认，否则改错了字母表顺序看不出来。
	if got := SniffMediaType(&ir.Media{Data: base64.StdEncoding.EncodeToString(raw)}); got != "image/jpeg" {
		t.Errorf("标准字母表载荷嗅探 = %q，want image/jpeg", got)
	}
}

// 判据 3：无填充（Raw*Encoding）的载荷靠截到 4 的整数倍自然解得出。
//
// 这条钉住「不为 RawStdEncoding 单独加一条分支」这个结论：填充位在载荷末尾，
// 而探测窗口只取头部 16 个字符，够不到那里。
func TestUnpaddedBase64NeedsNoSpecialCase(t *testing.T) {
	for name, enc := range map[string]*base64.Encoding{
		"raw-std": base64.RawStdEncoding,
		"raw-url": base64.RawURLEncoding,
	} {
		data := enc.EncodeToString(pngBytes())
		if strings.Contains(data, "=") {
			t.Fatalf("%s: 夹具编出了填充位，这条测不到无填充形态", name)
		}
		if got := SniffMediaType(&ir.Media{Data: data}); got != "image/png" {
			t.Errorf("%s: 嗅探 = %q，want image/png", name, got)
		}
	}
}

// webpBytes 是一份 RIFF 容器的 webp 头：格式标记在第 8-11 字节，
// 是魔数表里需要看得最深的一格。PNG 那类头 8 字节就能判的载荷测不到
// 「探测窗口被折行吃掉几个字符」这件事。
func webpBytes() []byte {
	return []byte("RIFF" + "\x24\x00\x00\x00" + "WEBP" + "VP8 0123")
}

// 判据 3b：折行不能吃掉探测窗口的有效字符数。
//
// 先截 16 个字符再剥空白的写法，遇到窗口内的折行就只剩 15 个有效字符，
// 向下取整到 12 后只解出 9 字节——够判 PNG，不够判 webp（要看到第 12 字节）。
// 于是一张 webp 图片被判成 audio/wav 或空串，前者更糟：MediaKindFor 把它
// 归到 BlockAudio，出站按音频块编码，上游拿到一个声称是音频的图片。
func TestDeepMagicSurvivesFoldingInsideTheWindow(t *testing.T) {
	std := base64.StdEncoding.EncodeToString(webpBytes())
	for name, data := range map[string]string{
		"plain":       std,
		"fold-at-4":   std[:4] + "\n" + std[4:],
		"fold-at-8":   std[:8] + "\r\n" + std[8:],
		"fold-at-12":  std[:12] + "\n" + std[12:],
		"dense-every": strings.Join([]string{std[:4], std[4:8], std[8:12], std[12:16], std[16:]}, "\r\n"),
	} {
		if got := SniffMediaType(&ir.Media{Data: data}); got != "image/webp" {
			t.Errorf("%s: 嗅探 = %q，want image/webp；判成 audio/* 会让出站按音频块编码", name, got)
		}
	}
	// 同一个容器里的 wav 仍要判成音频，否则「RIFF 一律当图片」也能让上面那条绿。
	wav := base64.StdEncoding.EncodeToString([]byte("RIFF" + "\x24\x00\x00\x00" + "WAVE" + "fmt 0123"))
	if got := SniffMediaType(&ir.Media{Data: wav}); got != "audio/wav" {
		t.Errorf("wav 嗅探 = %q，want audio/wav", got)
	}
}

// 判据 3c：有效字符数不是 4 的整数倍时必须向下取整。
//
// 探测窗口取 16 时，长载荷总能凑满 16 个有效字符，只有短载荷会露出这一步：
// 7 字节的无填充载荷编出 10 个字符，直接丢给解码器会因「长度不是 4 的倍数」
// 报错、返回空串，而这份载荷本身完全可判。
func TestShortPayloadIsTruncatedToAWholeBase64Group(t *testing.T) {
	data := base64.RawStdEncoding.EncodeToString([]byte("GIF89a\x00"))
	if len(data)%4 == 0 {
		t.Fatalf("夹具失效：%q 的长度正好是 4 的倍数，测不到取整那一步", data)
	}
	if got := SniffMediaType(&ir.Media{Data: data}); got != "image/gif" {
		t.Errorf("短载荷嗅探 = %q，want image/gif", got)
	}
}

// 判据 4：真判不出来时仍返回空串。
//
// 容错不能变成什么都认：一段解得出但不匹配任何魔数的字节、以及一段根本不是
// base64 的文本，都该落回空串交给调用方决定降级。
func TestUnknownPayloadStillYieldsEmpty(t *testing.T) {
	notMedia := base64.StdEncoding.EncodeToString([]byte("plain text, no magic at all"))
	if got := SniffMediaType(&ir.Media{Data: notMedia}); got != "" {
		t.Errorf("非媒体载荷嗅探 = %q，want 空串", got)
	}
	if got := SniffMediaType(&ir.Media{Data: "!!!!not base64 at all!!!!"}); got != "" {
		t.Errorf("非 base64 载荷嗅探 = %q，want 空串", got)
	}
	if got := SniffMediaType(&ir.Media{Data: ""}); got != "" {
		t.Errorf("空载荷嗅探 = %q，want 空串", got)
	}
	// 全是空白：剥完一个有效字符都不剩，不能当成「解出了零字节」去比魔数。
	if got := SniffMediaType(&ir.Media{Data: "\n\r\t   "}); got != "" {
		t.Errorf("全空白载荷嗅探 = %q，want 空串", got)
	}
}

// 判据 5：显式声明与后缀仍优先于魔数。
//
// 容错改的是最后那一级，前两级的顺序（显式声明比猜字节可信）不能被动到。
func TestExplicitDeclarationStillWinsOverMagic(t *testing.T) {
	png := base64.StdEncoding.EncodeToString(pngBytes())
	if got := SniffMediaType(&ir.Media{MediaType: "application/pdf", Data: png}); got != "application/pdf" {
		t.Errorf("显式声明被魔数盖掉了：%q", got)
	}
	if got := SniffMediaType(&ir.Media{Name: "report.pdf", Data: png}); got != "application/pdf" {
		t.Errorf("文件名后缀被魔数盖掉了：%q", got)
	}
}

// 判据 6：嗅探失败到降级的整条链。
//
// 这条是症状本身：折行载荷判不出类型时，AcceptsMedia 恒假，媒体块被
// DowngradeMedia 换成文本。前面几条断言的是嗅探结果，这条断言的是后果——
// 只测嗅探的话，「嗅探对了但降级仍然发生」这种形态看不出来。
func TestFoldedMediaSurvivesTheDowngradeGate(t *testing.T) {
	std := base64.StdEncoding.EncodeToString(pngBytes())
	folded := std[:8] + "\n" + std[8:]
	caps := Capabilities{Images: true, MediaTypes: []string{"image/png"}}

	m := &ir.Media{Data: folded}
	if !caps.AcceptsMedia(SniffMediaType(m)) {
		t.Fatal("折行图片过不了 media type 闸门；它会被 DowngradeMedia 换成文本备注，" +
			"客户端只看到「模型无视了我的图片」")
	}
	// 反面：真判不出类型时闸门仍该拦住——容错不能把闸门整个废掉。
	unknown := &ir.Media{Data: base64.StdEncoding.EncodeToString([]byte("no magic here at all"))}
	if caps.AcceptsMedia(SniffMediaType(unknown)) {
		t.Error("判不出类型的载荷过了闸门；上游的 mime 字段是必填的，谎报会被拒收")
	}
}
