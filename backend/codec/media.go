package codec

import (
	"encoding/base64"
	"fmt"
	"path"
	"strings"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// SniffMediaType 在上游/客户端没给 media type 时推断一个。
//
// 顺序是文件名后缀、URL 路径后缀、再是 base64 解出的头部魔数。先看名字：
// 后缀是发送方的显式声明，比猜字节更可信。全都无从判断时返回空串，
// 由调用方决定是照发还是降级——旧实现在这里硬编码 image/png，
// 把音频与 PDF 都谎报成图片，上游会拒收。
func SniffMediaType(m *ir.Media) string {
	if m == nil {
		return ""
	}
	if m.MediaType != "" {
		return m.MediaType
	}
	if ext := path.Ext(m.Name); ext != "" {
		if t := mediaTypeByExt(ext); t != "" {
			return t
		}
	}
	if m.URL != "" {
		// 查询串里可能也有点号，只看路径部分。
		u := m.URL
		if i := strings.IndexAny(u, "?#"); i >= 0 {
			u = u[:i]
		}
		if t := mediaTypeByExt(path.Ext(u)); t != "" {
			return t
		}
	}
	return sniffMagic(m.Data)
}

func mediaTypeByExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".pdf":
		return "application/pdf"
	case ".wav":
		return "audio/wav"
	case ".mp3":
		return "audio/mpeg"
	case ".ogg":
		return "audio/ogg"
	case ".flac":
		return "audio/flac"
	case ".txt":
		return "text/plain"
	case ".csv":
		return "text/csv"
	case ".json":
		return "application/json"
	default:
		return ""
	}
}

// sniffMagic 只解前几个字节：整段 base64 可能有几 MB，
// 而所有需要识别的格式都在头部 12 字节内可判。
//
// 两处容错缺一不可，缺了就把一份完全可解码的载荷判成「不知道是什么」，
// 而那个空串随后让 AcceptsMedia 恒假、媒体被整块降级成文本备注：
// 折行（很多 SDK 默认按 76 列断行）与 URL-safe 字母表（- 与 _ 代替 + 与 /）。
func sniffMagic(data string) string {
	head := base64Head(data, 16)
	if head == "" {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(head)
	if err != nil {
		// 标准库的两个字母表是独立的编码器，一个失败必须换另一个试。
		raw, err = base64.URLEncoding.DecodeString(head)
		if err != nil {
			return ""
		}
	}
	switch {
	case hasPrefix(raw, "\x89PNG"):
		return "image/png"
	case hasPrefix(raw, "\xff\xd8\xff"):
		return "image/jpeg"
	case hasPrefix(raw, "GIF8"):
		return "image/gif"
	case hasPrefix(raw, "%PDF"):
		return "application/pdf"
	case hasPrefix(raw, "RIFF"):
		// RIFF 是容器而不是格式：webp 图片与 wav 音频都以它开头，
		// 真正的格式写在第 8-11 字节。只看前四字节会把 webp 图片判成音频，
		// 于是 MediaKindFor 把它归到 BlockAudio，出站按音频块编码。
		if len(raw) >= 12 && string(raw[8:12]) == "WEBP" {
			return "image/webp"
		}
		return "audio/wav"
	case hasPrefix(raw, "OggS"):
		return "audio/ogg"
	case hasPrefix(raw, "fLaC"):
		return "audio/flac"
	case hasPrefix(raw, "ID3"), hasPrefix(raw, "\xff\xfb"):
		return "audio/mpeg"
	default:
		return ""
	}
}

// base64Head 取前 n 个非空白的 base64 字符，向下取整到 4 的整数倍。
//
// 边扫边剥而不是先截 n 个字符再剥空白：后者遇到一个折行就只剩 n-1 个有效字符，
// 向下取整后少解出 3 字节，而 RIFF/webp 这类要看到第 12 字节才能判。
// 也不整串 strings.Map：载荷可能几 MB，为读头部扫一整串正是 n 这个窗口要避免的。
func base64Head(data string, n int) string {
	buf := make([]byte, 0, n)
	for i := 0; i < len(data) && len(buf) < n; i++ {
		switch c := data[i]; c {
		case '\n', '\r', '\t', ' ':
			// base64 的折行与缩进不是载荷的一部分。
		default:
			buf = append(buf, c)
		}
	}
	// base64 每 4 字符解出 3 字节，不足 4 的那几个字符解不出来。
	return string(buf[:len(buf)/4*4])
}

func hasPrefix(raw []byte, want string) bool {
	return len(raw) >= len(want) && string(raw[:len(want)]) == want
}

// DowngradeMedia 把目标协议表达不了的媒体块改写成说明性文本。
//
// 降级而非丢弃：模型至少要知道「这里本来有个 PDF」，
// 否则用户提到「上面那份文件」时模型会答得莫名其妙。
// 也不整体拒收——一次附件不该打挂整轮对话。
func DowngradeMedia(b ir.Block) ir.Block {
	label := string(b.Type)
	var detail string
	if b.Media != nil {
		media := SniffMediaType(b.Media)
		switch {
		case b.Media.Name != "" && media != "":
			detail = fmt.Sprintf("%s, %s", b.Media.Name, media)
		case b.Media.Name != "":
			detail = b.Media.Name
		case media != "":
			detail = media
		}
		if b.Media.URL != "" {
			if detail != "" {
				detail += ", "
			}
			detail += b.Media.URL
		}
	}
	text := fmt.Sprintf("[%s attachment omitted: the target model cannot read it", label)
	if detail != "" {
		text += " — " + detail
	}
	return ir.Block{Type: ir.BlockText, Text: text + "]", CacheCtl: b.CacheCtl}
}

// MediaKindFor 给出 media type 所属的 IR 块类型，供解码侧把
// 协议自带的宽泛容器（如 responses 的 input_file）归到正确的类型。
func MediaKindFor(mediaType string) ir.BlockType {
	switch {
	case strings.HasPrefix(mediaType, "image/"):
		return ir.BlockImage
	case strings.HasPrefix(mediaType, "audio/"):
		return ir.BlockAudio
	case mediaType == "application/pdf":
		return ir.BlockDocument
	default:
		return ir.BlockFile
	}
}
