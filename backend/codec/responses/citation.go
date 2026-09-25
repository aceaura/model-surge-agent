package responses

import (
	"github.com/aceaura/model-surge-agent/backend/ir"
)

// citation.go 承载 output_text.annotations 与 IR Citation 之间的双向映射。
//
// 本协议的标注字段是平的（chat 形态嵌一层 url_citation 子对象），且没有
// cited_text 字段——只有 URL、标题与偏移量。允许无范围标注：定位不了
// 范围时偏移量留零值照发，客户端拿 URL 仍能渲染来源。

// orEmptyAnnotation 把 nil 指针换成零值，让调用方少写一层判空。
// annotation.added 帧的 annotation 键可以缺失（畸形上游），缺失时解出 nil。
func orEmptyAnnotation(a *annotation) *annotation {
	if a == nil {
		return &annotation{}
	}
	return a
}

// decodeAnnotations 线上标注 -> IR。
//
// 按 type 过滤：本协议的标注类型可能扩展（file_citation 等），认不出的
// 跳过而不是照 url 位解——别的类型的 url 语义不同，混进来会指向错误来源。
// 空 type 放行（部分兼容端省略）。本协议的标注同样以 URL 为来源身份，
// 空 URL 的那条连自己协议里都无从渲染，收下只会往下游传一条空壳。
func decodeAnnotations(as []annotation) []ir.Citation {
	if len(as) == 0 {
		return nil
	}
	out := make([]ir.Citation, 0, len(as))
	for _, a := range as {
		if a.Type != "" && a.Type != "url_citation" {
			continue
		}
		if a.URL == "" {
			continue
		}
		out = append(out, ir.Citation{
			URL:   a.URL,
			Title: a.Title,
			Start: a.StartIndex,
			End:   a.EndIndex,
		})
	}
	return ir.DedupeCitations(out)
}

// encodeAnnotations IR -> 线上标注。无法解析范围的条目保留（零偏移量），
// 判据与取舍见 citation.go 头注。
// 没有 URL 的引用跳过：url_citation 以 URL 为来源身份，空 url 的标注会让
// 客户端渲染出一个跳不动的引用。丢弃条数由调用方计入损耗注记。
func encodeAnnotations(text string, cs []ir.Citation) []annotation {
	if len(cs) == 0 {
		return nil
	}
	out := make([]annotation, 0, len(cs))
	for _, c := range cs {
		if !c.Portable() {
			continue
		}
		a := annotation{Type: "url_citation", URL: c.URL, Title: c.Title}
		if start, end, ok := ir.ResolveRange(text, c); ok {
			a.StartIndex = start
			a.EndIndex = end
		}
		out = append(out, a)
	}
	return out
}
