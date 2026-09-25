package anthropic

import (
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// citation.go 承载 text.citations 与 IR Citation 之间的双向映射。
//
// 本协议对引用的要求是四族里最严的：cited_text 必须真的出现在 text 里，
// start/end_char_index 必须指得准，缺一整条会被上游 400。所以编码方向
// 对无法定位的条目整条丢弃（有损由 DescribeLossy 报出），而不是编出
// 一个残缺形状把整轮请求送进拒绝。

// orEmptyCitation 把 nil 指针换成零值，让调用方少写一层判空。
// citations_delta 帧的 citation 键可以缺失（畸形上游），缺失时解出 nil。
func orEmptyCitation(c *citation) *citation {
	if c == nil {
		return &citation{}
	}
	return c
}

// decodeCitations 线上引用 -> IR。去重后交出：上游偶发重发同一标注，
// 累积进块会让聚合结果带上重复条目。
func decodeCitations(cs []citation) []ir.Citation {
	out := make([]ir.Citation, 0, len(cs))
	for _, c := range cs {
		out = append(out, ir.Citation{
			URL:            c.URL,
			Title:          c.Title,
			CitedText:      c.CitedText,
			Start:          c.StartCharIndex,
			End:            c.EndCharIndex,
			EncryptedIndex: c.EncryptedIndex,
		})
	}
	return ir.DedupeCitations(out)
}

// encodeCitations IR -> 线上引用。
//
// 跨协议来的引用常常只有 URL 没有范围（chat/responses 允许无范围标注），
// 而本协议两者都必填。先在块正文里回推 cited_text 与偏移量，回推不出来的
// 整条丢弃：编出去就是 400 拒整轮，丢掉只损失一条标注。
func encodeCitations(text string, cs []ir.Citation) []citation {
	out := make([]citation, 0, len(cs))
	for _, c := range cs {
		cited := ir.ResolveCitedText(text, c)
		if cited == "" {
			continue
		}
		start, end, ok := ir.ResolveRange(text, c)
		if !ok {
			continue
		}
		out = append(out, citation{
			Type:           "web_search_result_location",
			URL:            c.URL,
			Title:          c.Title,
			CitedText:      cited,
			EncryptedIndex: c.EncryptedIndex,
			StartCharIndex: start,
			EndCharIndex:   end,
		})
	}
	return out
}
