package anthropic

import (
	"encoding/json"

	"github.com/aceaura/model-surge-agent/backend/ir"
)

// citation.go 承载 text.citations 与 IR Citation 之间的双向映射。
//
// 官方 citations 是按 type 判别的五种形态并集（char_location /
// page_location / content_block_location / search_result_location /
// web_search_result_location）。同族往返一律以原文（ir.Citation.Raw）
// 原样带回；只有跨协议投影来的引用才需要重建，而重建只能落进
// web_search_result_location——那是唯一有 URL 槽位的形态。

// decodeCitations 解析 text 块的 citations 数组。非数组形态一律返回 nil：
// Anthropic 在 document / search_result 块上复用同一个键名承载
// {"enabled":bool} 配置对象，那不是引用。
func decodeCitations(raw json.RawMessage) []ir.Citation {
	if len(raw) == 0 || raw[0] != '[' {
		return nil
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil
	}
	return citationsToIR(elems)
}

// citationsConfig document / search_result 块上 citations 键承载的配置对象
// （官方 CitationsConfigParam，只有 enabled 一个键）。与 text 块上同名的引用
// 数组不是一回事，故单立一个类型，不复用 citationIn/citationOut。
type citationsConfig struct {
	Enabled bool `json:"enabled"`
}

// decodeCitationsConfig 解析 document 块上 citations 键承载的 {"enabled":bool}
// 配置对象。返回 nil 表示客户端根本没给这个键——与显式给了 false 语义不同，
// 故用指针保留三态。数组形态（text 块的引用）与非对象形态一律返回 nil：
// 那是另一种东西，误当配置会在编码时写出一个假的开关。
func decodeCitationsConfig(raw json.RawMessage) *bool {
	if len(raw) == 0 || raw[0] != '{' {
		return nil
	}
	var cfg citationsConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil
	}
	// enabled 缺键时官方语义为关闭，但「给了对象却没给 enabled」仍是一次显式
	// 表态，按 false 原样带回，不吞掉整个配置对象。
	return &cfg.Enabled
}

// citationsToIR 逐条以原文收，再把可跨协议的字段投影进 IR。
//
// 官方 union 有五种形态，其中只有 web_search_result_location 带 url、
// search_result_location 带 source；char_location / page_location /
// content_block_location 三种文档类引用靠 document_index 与页号/块下标/字符
// 下标定位，根本没有 URL。若整个数组只按 web_search 一种形态解，四种没有 url
// 的会被当成「空 URL 的废引用」静默丢光——五种进去只剩一种出来，既没有错误
// 也没有损耗注记，客户端看不到模型引了哪份文档的哪一段。
//
// 非对象元素（null、字符串、数字）直接跳过：它们不可能是引用，而 Unmarshal
// 进 struct 会成功并留下全零值，那样会凭空多出一条空引用。
func citationsToIR(elems []json.RawMessage) []ir.Citation {
	if len(elems) == 0 {
		return nil
	}
	out := make([]ir.Citation, 0, len(elems))
	for _, raw := range elems {
		if len(raw) == 0 || raw[0] != '{' {
			continue
		}
		var c citationIn
		if err := json.Unmarshal(raw, &c); err != nil {
			continue
		}
		url, title := c.URL, c.Title
		switch c.Type {
		case "search_result_location":
			// 该形态的来源 URL 在 source 键上：投影过去才能跨族表达，
			// 否则它会被当成文档类引用一起丢。
			url = c.Source
		case "char_location", "page_location", "content_block_location":
			// 文档类引用的标题在 document_title 键上。
			title = c.DocumentTitle
		}
		out = append(out, ir.Citation{
			URL: url, Title: title, CitedText: c.CitedText,
			Start: c.StartCharIndex, End: c.EndCharIndex,
			EncryptedIndex: c.EncryptedIndex,
			WireType:       c.Type, Raw: raw,
		})
	}
	return ir.DedupeCitations(out)
}

// encodeCitations IR -> Anthropic。
//
// 带 Raw 的一律原样带回：那是上游自己下发的形状，同族往返没有任何理由改写它，
// 而文档类引用的定位字段（document_index、页号、块下标、file_id）IR 之外无处可放，
// 逐字段重建等于伪造。
//
// 没有 Raw 的（跨协议投影来的引用）只能落进 web_search_result_location。
// cited_text 是该形态的必填字段，缺失时按范围从正文反推；反推不出来就整条丢弃
// ——带空 cited_text 发出去上游会 400，丢一条引用好过整轮被拒。cited_text
// 已给但正文里找不到时**不**丢：官方这一形态不要求范围，上游拿 cited_text
// 自己定位。
func encodeCitations(text string, cs []ir.Citation) []json.RawMessage {
	if len(cs) == 0 {
		return nil
	}
	out := make([]json.RawMessage, 0, len(cs))
	for _, c := range cs {
		if len(c.Raw) > 0 {
			out = append(out, c.Raw)
			continue
		}
		cited := ir.ResolveCitedText(text, c)
		if cited == "" {
			continue
		}
		b, err := json.Marshal(citationOut{
			Type: "web_search_result_location", URL: c.URL, Title: c.Title,
			CitedText: cited, EncryptedIndex: c.EncryptedIndex,
		})
		if err != nil {
			continue
		}
		out = append(out, b)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// countUnresolvableCitations 数出 encodeCitations 会整条丢弃的引用：没有 Raw
// 可透传、又反推不出 cited_text 的那些。判据必须与 encodeCitations 的丢弃分支
// 逐字对齐（同样是 len(Raw)==0 且 ResolveCitedText 返回空），否则诊断计数会与
// 实际编码漂移——报了没丢、或丢了没报，两种都让「从不静默丢弃」失效。
func countUnresolvableCitations(text string, cs []ir.Citation) int {
	n := 0
	for _, c := range cs {
		if len(c.Raw) > 0 {
			continue
		}
		if ir.ResolveCitedText(text, c) == "" {
			n++
		}
	}
	return n
}

// countResponseDroppedCitations 扫非流式响应，数出 encodeBlock 会整条丢弃的
// 引用总数。判据与流式侧同源（逐文本块调 countUnresolvableCitations），两条
// 路径报同一件事、用同一措辞，非流式响应不会因为走了另一条编码路就漏报。
func countResponseDroppedCitations(resp *ir.Response) int {
	if resp == nil {
		return 0
	}
	n := 0
	for _, b := range resp.Content {
		n += countUnresolvableCitations(b.Text, b.Citations)
	}
	return n
}
