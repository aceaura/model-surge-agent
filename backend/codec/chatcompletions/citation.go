package chatcompletions

import (
	"unicode/utf8"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// citation.go 承载 message.annotations 与 IR Citation 之间的双向映射。
//
// 本协议的标注挂在消息上而非文本块上，偏移量相对于整条消息 content 的
// 拼接文本。IR 的引用挂在块上，因此：
//   - 解码方向要把消息级标注落到某一个文本块上（attachCitations）；
//   - 编码方向要把各块内的偏移量平移回拼接文本坐标系（shiftCitations）。

// attachCitations 把一批标注挂到最后一个文本块上。
//
// 选最后一个而不是第一个：本协议的标注随正文增量在消息尾部到达，
// 多块正文时它描述的更可能是最近输出的那段。偏移量不重新解析——
// 上游给的就是相对拼接文本的坐标，块内定位交给编码方向的 ResolveRange。
// 没有任何文本块时整批丢弃：标注没有正文可依附，留着会让下游
// 在一个不存在的块上找偏移量。
func attachCitations(blocks []ir.Block, cs []ir.Citation) []ir.Block {
	if len(cs) == 0 {
		return blocks
	}
	for i := len(blocks) - 1; i >= 0; i-- {
		if blocks[i].Type == ir.BlockText {
			blocks[i].Citations = ir.DedupeCitations(append(blocks[i].Citations, cs...))
			return blocks
		}
	}
	return blocks
}

// shiftCitations 把块内坐标的引用平移到消息拼接文本坐标系。
// prefix 是该块之前所有文本块拼起来的长度（rune 数），own 是本块正文。
//
// 先在自己的块里解析范围再平移：块内定位是精确的（cited_text 就出自
// 这段正文），直接在拼接文本里搜可能命中别块的相同片段。
// 解析不出来时保留原范围（多为跨协议带来的、已是拼接坐标的值）。
func shiftCitations(cs []ir.Citation, prefix, own string) []ir.Citation {
	if len(cs) == 0 {
		return nil
	}
	off := utf8.RuneCountInString(prefix)
	out := make([]ir.Citation, 0, len(cs))
	for _, c := range cs {
		shifted := c
		if start, end, ok := ir.ResolveRange(own, c); ok {
			shifted.Start = start + off
			shifted.End = end + off
		}
		if shifted.CitedText == "" {
			shifted.CitedText = ir.ResolveCitedText(own, c)
		}
		out = append(out, shifted)
	}
	return out
}

// decodeAnnotations 线上标注 -> IR。
//
// 只认 url_citation 形态：判别看子对象而不是 type 字符串，因为部分兼容端
// 省略 type 却给了 url_citation。file_citation 等其他类型没有这个子对象，
// 自然被跳过。Chat 的标注以 URL 为来源身份，没有 URL 的那条连自己协议里
// 都无从渲染，收下只会往下游传一条空壳。
func decodeAnnotations(as []annotation) []ir.Citation {
	if len(as) == 0 {
		return nil
	}
	out := make([]ir.Citation, 0, len(as))
	for _, a := range as {
		uc := a.URLCitation
		if uc == nil || uc.URL == "" {
			continue
		}
		out = append(out, ir.Citation{
			URL:       uc.URL,
			Title:     uc.Title,
			CitedText: uc.CitedText,
			Start:     uc.StartIndex,
			End:       uc.EndIndex,
		})
	}
	return ir.DedupeCitations(out)
}

// encodeAnnotations IR -> 线上标注。
//
// 与 Anthropic 不同，本协议允许无范围标注（start/end 为零值照发）：
// 客户端拿 URL 与 cited_text 就能渲染来源，偏移量只影响高亮。
// 所以定位不了范围时不丢条目，只在能解析时补出偏移量。
// 没有 URL 的引用跳过：url_citation 以 URL 为来源身份，写一条空 url 的标注
// 会让客户端渲染出一个跳不动的引用。丢弃条数由调用方计入损耗注记。
func encodeAnnotations(text string, cs []ir.Citation) []annotation {
	if len(cs) == 0 {
		return nil
	}
	out := make([]annotation, 0, len(cs))
	for _, c := range cs {
		if !c.Portable() {
			continue
		}
		uc := &urlCitation{
			URL:       c.URL,
			Title:     c.Title,
			CitedText: ir.ResolveCitedText(text, c),
		}
		if start, end, ok := ir.ResolveRange(text, c); ok {
			uc.StartIndex = start
			uc.EndIndex = end
		}
		out = append(out, annotation{Type: "url_citation", URLCitation: uc})
	}
	return out
}
