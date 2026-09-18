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
func sniffMagic(data string) string {
	if data == "" {
		return ""
	}
	const probe = 16
	head := data
	if len(head) > probe {
		// base64 每 4 字符解出 3 字节，截到 4 的整数倍才能解。
		head = head[:probe/4*4]
	}
	raw, err := base64.StdEncoding.DecodeString(head)
	if err != nil {
		return ""
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
