package codec

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

func b64(raw string) string { return base64.StdEncoding.EncodeToString([]byte(raw)) }

// 类型判断的可信度顺序是显式声明 > 文件名 > URL 后缀 > 字节魔数。
// 旧实现在缺失时硬编码 image/png，把音频与 PDF 都谎报成图片。
func TestSniffMediaTypePrefersExplicitThenNameThenMagic(t *testing.T) {
	cases := []struct {
		name  string
		media ir.Media
		want  string
	}{
		{"explicit wins over conflicting name",
			ir.Media{MediaType: "audio/wav", Name: "a.png"}, "audio/wav"},
		{"name beats magic",
			ir.Media{Name: "spec.pdf", Data: b64("\x89PNG....")}, "application/pdf"},
		{"url suffix",
			ir.Media{URL: "https://example.com/a/b.webp"}, "image/webp"},
		{"url suffix ignores query string",
			ir.Media{URL: "https://example.com/a.mp3?token=x.png"}, "audio/mpeg"},
		{"magic png", ir.Media{Data: b64("\x89PNG\r\n\x1a\n....")}, "image/png"},
		{"magic jpeg", ir.Media{Data: b64("\xff\xd8\xff\xe0....")}, "image/jpeg"},
		{"magic pdf", ir.Media{Data: b64("%PDF-1.7 ....")}, "application/pdf"},
		{"magic wav", ir.Media{Data: b64("RIFF....WAVE")}, "audio/wav"},
		{"magic ogg", ir.Media{Data: b64("OggS............")}, "audio/ogg"},
		{"magic flac", ir.Media{Data: b64("fLaC............")}, "audio/flac"},
		{"magic mp3 id3", ir.Media{Data: b64("ID3\x04............")}, "audio/mpeg"},
		{"unknown stays empty", ir.Media{Data: b64("nothing recognizable")}, ""},
		{"empty media stays empty", ir.Media{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SniffMediaType(&c.media); got != c.want {
				t.Errorf("SniffMediaType(%+v) = %q, want %q", c.media, got, c.want)
			}
		})
	}
}

func TestSniffMediaTypeNilIsEmpty(t *testing.T) {
	if got := SniffMediaType(nil); got != "" {
		t.Errorf("SniffMediaType(nil) = %q, want empty", got)
	}
}

// 无法解码的 base64 不能让嗅探 panic：客户端会送来截断的数据。
func TestSniffMediaTypeToleratesBrokenBase64(t *testing.T) {
	if got := SniffMediaType(&ir.Media{Data: "!!!not base64!!!"}); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// 降级要留下足够的线索：用户提到「上面那份文件」时模型得答得上来。
func TestDowngradeMediaKeepsIdentifyingDetail(t *testing.T) {
	got := DowngradeMedia(ir.Block{
		Type:     ir.BlockDocument,
		Media:    &ir.Media{MediaType: "application/pdf", Name: "quarterly.pdf"},
		CacheCtl: "ephemeral",
	})
	if got.Type != ir.BlockText {
		t.Fatalf("type = %q, want text", got.Type)
	}
	for _, want := range []string{"document", "quarterly.pdf", "application/pdf"} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("降级文本缺少 %q: %s", want, got.Text)
		}
	}
	// cache_control 是块的属性而非内容，降级后要跟着走，否则缓存前缀断裂。
	if got.CacheCtl != "ephemeral" {
		t.Errorf("cache_ctl = %q, want ephemeral", got.CacheCtl)
	}
}

func TestDowngradeMediaMentionsURLWhenThereIsNoName(t *testing.T) {
	got := DowngradeMedia(ir.Block{
		Type:  ir.BlockFile,
		Media: &ir.Media{URL: "https://example.com/a.bin"},
	})
	if !strings.Contains(got.Text, "https://example.com/a.bin") {
		t.Errorf("降级文本缺少 URL: %s", got.Text)
	}
}

func TestDowngradeMediaWithoutPayloadStillSaysSomething(t *testing.T) {
	got := DowngradeMedia(ir.Block{Type: ir.BlockAudio})
	if got.Type != ir.BlockText || !strings.Contains(got.Text, "audio") {
		t.Errorf("got %+v, want a text block mentioning audio", got)
	}
}

func TestMediaKindForRoutesByMediaType(t *testing.T) {
	cases := map[string]ir.BlockType{
		"image/png":        ir.BlockImage,
		"image/webp":       ir.BlockImage,
		"audio/wav":        ir.BlockAudio,
		"audio/mpeg":       ir.BlockAudio,
		"application/pdf":  ir.BlockDocument,
		"text/csv":         ir.BlockFile,
		"application/json": ir.BlockFile,
		"":                 ir.BlockFile,
	}
	for media, want := range cases {
		if got := MediaKindFor(media); got != want {
			t.Errorf("MediaKindFor(%q) = %q, want %q", media, got, want)
		}
	}
}
