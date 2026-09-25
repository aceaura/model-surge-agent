package codec

import (
	"fmt"
	"sort"
	"strings"

	"github.com/aceaura/model-surge-agent/backend/ir"
	"github.com/aceaura/model-surge-agent/backend/textsafe"
)

// foreignSigPrefixes 是别家协议的推理密文特征前缀。
//
// 命中即剥离，即使 SignatureFrom 声称同族：声明可能来自伪造，也可能来自
// 本服务早期版本写下的历史数据。把别家的密文发给上游会拿到不可重试的 400，
// 而不可重试意味着换目标也救不回来。
var foreignSigPrefixes = map[string]string{
	// OpenAI/Codex 系的加密推理载荷，归属 responses 协议。
	"gAAAA": ProtocolResponses,
}

// prefixSaysForeign 按密文特征前缀判断签名是否属于别家。
//
// 归属协议必须参与判定：早先这里只看前缀不看协议名，于是 responses 自己的
// gAAAA 密文发回 responses 上游时也被当异族剥掉，该协议上的多轮推理
// 永远拿不到签名，而上游又要求带签名才接受带思考的续写。
func prefixSaysForeign(signature, name string) bool {
	for p, owner := range foreignSigPrefixes {
		if strings.HasPrefix(signature, p) {
			return owner != name
		}
	}
	return false
}

// DescribeLossy 按出站能力位推导本次编码会丢弃哪些 IR 字段。
//
// 只看 IR 与 Caps，不看编码结果：编码器丢字段是无声的，事后从请求体反推
// 会漏掉「本来就没编出去」的情形。返回值已去重并排序，令同一字段在多条
// 消息上被丢弃只报一条，且顺序稳定便于落库比对。
//
// 无丢弃时返回 nil。
func DescribeLossy(req *ir.Request, name string, caps Capabilities) []string {
	if req == nil {
		return nil
	}
	notes := map[string]string{}
	note := func(field, why string) {
		notes[field] = fmt.Sprintf("dropped %s (%s cannot express it: %s)", field, name, why)
	}
	// filled 与 note 分开：兜底不是丢弃，套上 "dropped" 的措辞会让读者
	// 以为客户端给的值被扔了，而真相是它根本没给、这个值是本服务加的。
	filled := func(field, why string) {
		notes[field] = fmt.Sprintf("filled in %s (%s requires it: %s)", field, name, why)
	}
	// unreturned 是第三种形态：字段照原样发给了上游、上游也会照办，
	// 只是结果回不到客户端手里。既不是 dropped（没丢，发出去了）
	// 也不是 filled（没兜底）。措辞混用会让读者以为换个目标就有。
	unreturned := func(field, why string) {
		notes[field] = fmt.Sprintf("forwarded %s but the result is not returned (%s)", field, why)
	}
	// rewrote 是第四种形态：这一维承载不了，但没丢——本服务把它改写成了
	// 目标协议里能表达的另一种形状。措辞与 dropped 分开，因为读者对这两者
	// 的下一步动作不同：dropped 要考虑换目标，rewrote 要考虑上游会怎么读。
	rewrote := func(field, why string) {
		notes[field] = fmt.Sprintf("rewrote %s (%s cannot express it: %s)", field, name, why)
	}

	if len(req.Tools) > 0 && !caps.Tools {
		note("tools", "no tool calling")
	}
	if !caps.ToolResultError && hasFailedToolResult(req) {
		rewrote("tool_result.is_error",
			fmt.Sprintf("prefixed the content with %q", toolErrorPrefix))
	}
	// 畸形工具参数与协议无关：任何出站都会发生处置（对象槽位挪进
	// RawArgsKey，字符串槽位原样透出但工具侧多半也解析不了），
	// 一律报告，只是措辞按槽位形态分开——两种后果不同，读者要改的
	// 地方也不同。
	if bad := CountMalformedToolArgs(req); bad > 0 {
		if caps.ToolInputObject {
			notes["tool arguments"] = fmt.Sprintf(
				"rewrapped %d tool call argument(s): malformed or non-object JSON moved to %s, the tool will not receive its parameters",
				bad, ir.RawArgsKey)
		} else {
			notes["tool arguments"] = fmt.Sprintf(
				"passed through %d malformed tool call argument(s) verbatim: the tool will fail to parse them", bad)
		}
	}
	if req.TopK != nil && !caps.TopK {
		note("top_k", "no top_k parameter")
	}
	if len(req.StopSequences) > 0 && !caps.StopSequences {
		note("stop_sequences", "no stop sequence parameter")
	}
	// 只有明确开启才算丢失：明确关闭在不支持推理的协议上本就是要的结果，
	// 没提则什么都没被丢。
	if req.Thinking.On() && !caps.Thinking {
		note("thinking", "no reasoning mode")
	}
	describeParamsLossy(req, caps, note, filled, unreturned)

	describeBlocksLossy(req.System, name, caps, note)
	for _, m := range req.Messages {
		describeBlocksLossy(m.Content, name, caps, note)
	}

	// 历史里的托管工具块（server_tool_use / web_search_tool_result）：三个
	// 外族出站编码器都整块跳过。两种块型成对出现、一起丢反而不撕毁
	// tool_use/tool_result 配平，上游不会拒——但模型看不到自己上一轮让
	// 网关搜了什么、搜回了哪些页面，只能重新搜一遍。
	// anthropic 一律不报：同族原样往返，报了就是谎报。
	if name != ProtocolAnthropic {
		if calls, results := countRequestServerTools(req); calls > 0 || results > 0 {
			notes["server tool blocks"] = ServerToolDropNote(calls, results)
		}
	}

	if !caps.Citations {
		if n := ir.CountCitations(req); n > 0 {
			// 历史消息里的来源标注会被抹掉。读者能做的是别指望模型在后续轮次里
			// 复述出处——它看到的正文已经不带任何来源了。
			notes["citations"] = fmt.Sprintf(
				"dropped %d citation(s): upstream protocol has no slot for source annotations", n)
		}
	} else if name != ProtocolAnthropic {
		// 有槽位，但槽位以 URL 为来源身份：Anthropic 的文档类引用只有
		// document_index 与页/块/字符下标，装不进去。caps.Citations 是个
		// 整族布尔量，看不见这种「逐条装不下」的损耗，必须单独数。
		// anthropic 不报：五种形态它都装得下（同族往返走 Raw 原样带回）。
		if n := ir.CountNonPortableCitations(req); n > 0 {
			notes["citations"] = CitationDropNote(n)
		}
	}

	if len(notes) == 0 {
		return nil
	}
	out := make([]string, 0, len(notes))
	for _, v := range notes {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func describeBlocksLossy(blocks []ir.Block, name string, caps Capabilities, note func(field, why string)) {
	for _, b := range blocks {
		if b.CacheCtl != "" && !caps.CacheControl {
			note("cache_control", "no per-block cache markers")
		}
		switch {
		case b.Type == ir.BlockThinking && b.Thinking != nil:
			if b.Thinking.Redacted {
				// 与 Thinking 能力位无关：载荷是不可解读的密文，
				// 任何出站协议都表达不了，包括同族的 Anthropic。
				note("redacted_thinking", "encrypted reasoning payload cannot be re-encoded")
				continue
			}
			if !caps.Thinking {
				note("thinking blocks", "no reasoning content")
				continue
			}
			if b.Thinking.Signature == "" {
				continue
			}
			if why, drop := sigDropReason(b.Thinking, name, caps); drop {
				note("thinking signature", why)
			}

		case b.Type.IsMedia():
			if !caps.Images {
				note(string(b.Type)+" blocks", "no media input")
				continue
			}
			if b.Type == ir.BlockImage {
				describeImageLossy(b.Media, caps, note)
			}
			if !caps.AcceptsMedia(SniffMediaType(b.Media)) {
				note(string(b.Type)+" blocks", "unsupported media type, downgraded to text")
			}

		case b.Type == ir.BlockToolUse, b.Type == ir.BlockToolResult:
			if !caps.Tools {
				note("tool blocks", "no tool calling")
			}
			if why, drop := toolSigDropReason(b.ToolUse, name, caps); drop {
				note("tool call signature", why)
			}
			if b.ToolResult != nil {
				describeBlocksLossy(b.ToolResult.Content, name, caps, note)
			}
		}
	}
}

// describeImageLossy 报一张图片相对目标协议丢掉的三维：detail 档位、
// file_id 引用、以及整个部件没有任何可投递载荷。与能力位判定同一出处，
// 编码器的跳过判据（HasPayload）与这里的「确实丢了」口径一致。
//
// 媒体类型不被接受时不报这三条：那种图片本来就要整体降级成文本，
// 三条细则叠上去只会盖掉真正的原因。
func describeImageLossy(m *ir.Media, caps Capabilities, note func(field, why string)) {
	if m == nil || !caps.AcceptsMedia(SniffMediaType(m)) && m.FileID == "" {
		return
	}
	if m.Detail != "" && !caps.ImageDetail {
		// 档位决定上游怎么切图、进而决定输入 token 计费。丢掉之后上游一律
		// 按自己的默认档处理，账单上看得出、请求里看不出。
		note("image detail", "no detail slot, the upstream will tile it at its own default level")
	}
	if m.HasPayload() {
		return
	}
	if m.FileID != "" {
		if caps.ImageFileRef {
			// 只凭文件引用即可投递，本目标装得下。
			return
		}
		// 图片字节从未内联进请求体，本服务也不代取上游文件服务，所以这一维
		// 装不下就是彻底没了——与 URL 那种「换成 base64 即可」不同。
		note("image file reference",
			"the image slot cannot point at a file-service id, and the bytes were never inlined, so they cannot be recovered here")
		return
	}
	// 三个载体全空：照编上去是缺必填键的形状（anthropic 的 base64 source
	// 缺 media_type/data，OpenAI 两系写出 url:"" 或连 image_url 键都没有），
	// 上游 400 拒整轮。
	note("image payload",
		"the part carries no payload the target protocol can express (no base64, no URL, no usable file reference); an empty image part would be rejected upstream")
}

// sigDropReason 回答签名是否要剥离，以及为什么。
func sigDropReason(t *ir.Thinking, name string, caps Capabilities) (string, bool) {
	if !caps.ThinkingSig {
		return "no signed reasoning", true
	}
	if prefixSaysForeign(t.Signature, name) {
		return "signature carries another vendor's ciphertext", true
	}
	if t.SignatureFrom != name {
		return "signature is only valid within its own protocol family", true
	}
	return "", false
}

// ForeignEventSignature 判断一条流式签名增量是否要剥离，并给出说明。
//
// 与请求侧共用 foreignSigPrefixes 与同族判定：两侧结论不一致时，
// 同一份推理内容在流式与非流式两条路上会得到不同处置，客户端把流式
// 存下来的历史回放成非流式请求就会被上游拒收。
//
// 来源为空按异族处理：签名无从验证时放行的代价是客户端把一段它验不了的
// 密文存进历史，下一轮整个请求被拒；丢掉的代价只是这轮少一段签名。
func ForeignEventSignature(signature, from, name string) (string, bool) {
	if signature == "" {
		return "", false
	}
	if prefixSaysForeign(signature, name) {
		return responseSigNote(name, "signature carries another vendor's ciphertext"), true
	}
	if from != name {
		return responseSigNote(name, "signature is only valid within its own protocol family"), true
	}
	return "", false
}

func responseSigNote(name, why string) string {
	return fmt.Sprintf("dropped thinking signature from the response (%s cannot express it: %s)", name, why)
}

// toolSigDropReason 是请求侧的判据，与 sigDropReason 平行。
//
// 返回的 why 是裸理由（由 note 加前缀），而 ForeignToolSignature 返回的是
// 完整的响应侧说明——两侧格式不同但判据同一处，改一边不会漏另一边。
func toolSigDropReason(use *ir.ToolUse, name string, caps Capabilities) (string, bool) {
	if use == nil || use.Signature == "" {
		return "", false
	}
	if !caps.ToolCallSig {
		return "no signature field on function calls", true
	}
	if prefixSaysForeign(use.Signature, name) {
		return "signature carries another vendor's ciphertext", true
	}
	if use.SignatureFrom != name {
		return "signature is only valid within its own protocol family", true
	}
	return "", false
}

// ForeignToolSignature 判断一次工具调用附带的推理签名是否要剥离。
//
// 与 ForeignEventSignature 同一套判据（前缀说异族、或来源不是本协议），
// 但说明里点明丢的是工具调用那一处：一条响应里可以既有思考块的签名又有
// 若干次调用各自的签名，措辞不分开的话运维看不出丢的是哪个。
//
// sigSupported 为假表示本协议的函数调用根本没有签名字段（除 gemini 外
// 其余三家都是），此时与来源无关一律剥离。
func ForeignToolSignature(signature, from, name string, sigSupported bool) (string, bool) {
	if signature == "" {
		return "", false
	}
	if !sigSupported {
		return toolSigNote(name, "no signature field on function calls"), true
	}
	if prefixSaysForeign(signature, name) {
		return toolSigNote(name, "signature carries another vendor's ciphertext"), true
	}
	if from != name {
		return toolSigNote(name, "signature is only valid within its own protocol family"), true
	}
	return "", false
}

func toolSigNote(name, why string) string {
	return fmt.Sprintf("dropped tool call reasoning signature (%s cannot express it: %s)", name, why)
}

// ResponseSignatureUnsupported 给没有签名字段的协议用，措辞与其它响应侧说明一致。
func ResponseSignatureUnsupported(name string) string {
	return responseSigNote(name, "no signed reasoning")
}

// DescribeResponseSignatureLoss 推导非流式响应编码会丢弃哪些推理签名。
//
// sigSupported 为假表示本协议根本没有签名字段（如 chat_completions），
// 此时所有签名都表达不了，与来源无关。
//
// 与流式路径共用 responseSigNote：同一件事在两条路径上措辞不同，
// 按说明检索流水的人会以为是两种故障。
func DescribeResponseSignatureLoss(resp *ir.Response, name string, sigSupported bool) []string {
	if resp == nil {
		return nil
	}
	var notes []string
	for _, b := range resp.Content {
		if b.Type != ir.BlockThinking || b.Thinking == nil || b.Thinking.Signature == "" {
			continue
		}
		if !sigSupported {
			notes = append(notes, responseSigNote(name, "no signed reasoning"))
			continue
		}
		if note, drop := ForeignEventSignature(b.Thinking.Signature, b.Thinking.SignatureFrom, name); drop {
			notes = append(notes, note)
		}
	}
	return DedupeNotes(notes)
}

// DescribeResponseToolSignatureLoss 推导响应编码会丢弃哪些工具调用签名。
//
// 与 thinking 那条分开：gemini 上游的工具调用带签名，而三个入站协议的
// tool_use 都没有这个字段，于是这一位在回客户端时必然被剥离。此前它在
// 解码时就消失，有损列里查不到任何痕迹——运维无从知道上游给过推理凭据。
//
// toolSigSupported 为假表示本协议的函数调用没有签名字段（除 gemini 外全是）。
func DescribeResponseToolSignatureLoss(resp *ir.Response, name string, toolSigSupported bool) []string {
	if resp == nil {
		return nil
	}
	var notes []string
	for _, b := range resp.Content {
		if b.Type != ir.BlockToolUse {
			continue
		}
		if note, drop := ForeignToolSignature(signatureOf(b.ToolUse),
			signatureFromOf(b.ToolUse), name, toolSigSupported); drop {
			notes = append(notes, note)
		}
	}
	return DedupeNotes(notes)
}

// DescribeResponseToolArgsLoss 推导响应编码会对畸形工具参数做什么。
//
// 参数不是合法 JSON 对象（max_tokens 截断是最常见来源）时两条路径都不允许
// 静默清空成 {}——那会让工具不带参数执行，是一次真实副作用。对象槽位协议
// （anthropic 的 input）把原文挪进 ir.RawArgsKey 键位；字符串槽位协议
// （chat / responses 的 arguments）原样透传。两者都出说明，措辞不同：
// 读者要改的地方不同，一个看键位，一个看原文。
//
// 与各编码器的编码分支用同一判定（ir.NormalizeToolInput），扫描结果即实编结果。
func DescribeResponseToolArgsLoss(resp *ir.Response, objectSlot bool) []string {
	if resp == nil {
		return nil
	}
	bad := 0
	for _, b := range resp.Content {
		if b.Type == ir.BlockToolUse && b.ToolUse != nil {
			if _, ok := ir.NormalizeToolInput([]byte(b.ToolUse.Input)); !ok {
				bad++
			}
		}
	}
	if bad == 0 {
		return nil
	}
	if objectSlot {
		return []string{ir.RewrapNote(bad)}
	}
	return []string{ir.RawArgsPassNote(bad)}
}

// CountMalformedToolArgs 统计请求消息里参数不是合法 JSON 对象的工具调用数。
// 判据与 ir.NormalizeToolInput 同源：非法 JSON（多为截断）与合法非对象都算。
func CountMalformedToolArgs(req *ir.Request) int {
	if req == nil {
		return 0
	}
	bad := 0
	count := func(blocks []ir.Block) {
		for _, b := range blocks {
			if b.Type == ir.BlockToolUse && b.ToolUse != nil {
				if _, ok := ir.NormalizeToolInput([]byte(b.ToolUse.Input)); !ok {
					bad++
				}
			}
		}
	}
	count(req.System)
	for _, m := range req.Messages {
		count(m.Content)
	}
	return bad
}

func signatureOf(use *ir.ToolUse) string {
	if use == nil {
		return ""
	}
	return use.Signature
}

func signatureFromOf(use *ir.ToolUse) string {
	if use == nil {
		return ""
	}
	return use.SignatureFrom
}

// DedupeNotes 把累加的说明去重并排序，供响应侧编码器的 Notes 出口使用。
// 空输入返回 nil，保持「无丢弃则字段不出现」的语义。
func DedupeNotes(notes []string) []string {
	if len(notes) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(notes))
	out := make([]string, 0, len(notes))
	for _, n := range notes {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// ForeignSignature 判断签名是否属于别家协议，出站 codec 编码前用它决定是否剥离。
//
// 与 DescribeLossy 共用一套判定：诊断说丢了而编码器实际发了出去，
// 比不诊断更糟——排查的人会照着诊断去找一个不存在的原因。
func ForeignSignature(t *ir.Thinking, name string) bool {
	if t == nil || t.Signature == "" {
		return false
	}
	if prefixSaysForeign(t.Signature, name) {
		return true
	}
	return t.SignatureFrom != name
}

// describeParamsLossy 报这一批调参字段的丢弃。
//
// 判据统一：只有客户端**给了**的字段才报（指针非 nil / 字符串非空 /
// 集合非空）。没给的字段什么都没被丢，报了会让每个普通请求都拖着一串
// 无意义的说明，真正的丢弃反而看不见。
//
// 这一批一律只报不拒：拒绝会把一个能用的回答换成零回答，而目标协议是
// 调度层按策略选的、客户端无从预知，让它为一个自己控制不了的路由结果
// 吃 400，故障归因方向是错的。要强制可以用模型配置的 overrides。
func describeParamsLossy(req *ir.Request, caps Capabilities, note, filled, unreturned func(field, why string)) {
	if !caps.Penalties {
		if req.PresencePenalty != nil {
			note("presence_penalty", "no presence penalty parameter")
		}
		if req.FrequencyPenalty != nil {
			note("frequency_penalty", "no frequency penalty parameter")
		}
	}
	if req.Seed != nil && !caps.Seed {
		note("seed", "no seed parameter, results are not reproducible")
	}
	if !caps.ImageDetail && hasImageDetail(req) {
		// 说清后果是计费：笼统的 dropped detail 读不出「账单会变」这一点。
		note("image_url.detail",
			"no image detail level, the upstream default applies and may be billed differently")
	}
	if req.Candidates != nil && !caps.Candidates {
		// 措辞要说清后果：客户端按数组取第二个候选会越界，
		// 笼统的 dropped n 读不出这一点。
		//
		// 目标支持该参数时这里刻意不报：上游会真的回多路，实际丢了
		// 几路要到响应侧才知道，那个数字比「可能会丢」有用。两格互斥。
		note("n", "no multi-candidate parameter, only one candidate will be returned")
	}
	if !caps.LogProbs {
		if req.LogProbs != nil {
			note("logprobs", "no log probability parameter")
		}
		if req.TopLogProbs != nil {
			note("top_logprobs", "no log probability parameter")
		}
	} else if req.LogProbs != nil || req.TopLogProbs != nil {
		// 目标支持、上游会算，但本服务的中立表示不承载按 token 的概率，
		// 解码时丢掉。与「目标不支持」是两件事，措辞必须不同：
		// 前者客户端换个目标就有，后者换谁都没有。
		unreturned("logprobs", "per-token probabilities are not carried across protocols")
	}
	if len(req.LogitBias) > 0 && !caps.LogitBias {
		// 不翻译：偏置的键是 token id，词表随模型而变，跨模型重映射
		// 没有正确答案。
		note("logit_bias", "no logit bias parameter")
	}
	if req.ServiceTier != "" && !caps.ServiceTier {
		note("service_tier", "no service tier parameter")
	}
	if req.ParallelToolCalls != nil && !caps.ParallelToolCalls {
		note("parallel_tool_calls", "no parallel tool call switch")
	}
	if req.ResponseFormat != nil {
		switch {
		case !caps.ResponseFormat:
			// 后果最重的一条：客户端会按 JSON 解析响应，拿到自然语言就崩。
			note("response_format", "no structured output parameter, the response may not be JSON")
		case req.ResponseFormat.Kind == ir.ResponseFormatSchema && !caps.ResponseSchema:
			note("response_format.schema", "structured output is supported but not schema constraints, downgraded to plain JSON")
		}
	}
	if req.Verbosity != "" && !caps.Verbosity {
		note("verbosity", "no verbosity parameter")
	}
	if len(req.Include) > 0 && !caps.Include {
		note("include", "no include parameter")
	}
	if req.Truncation != "" && !caps.Truncation {
		note("truncation", "no upstream-side truncation parameter")
	}
	if len(req.ClientMetadata) > 0 && !caps.ClientMetadata {
		note("metadata", "no client metadata parameter")
	}
	if req.MaxTokens <= 0 && caps.RequiresMaxTokens && caps.DefaultMaxTokens > 0 {
		// 兜底不是丢弃，但同样是本服务改了客户端没给的东西，必须留痕：
		// 否则长回答在一个客户端从未设过的上限处被截断，无从查证。
		filled("max_tokens", fmt.Sprintf("the client gave none, defaulted to %d", caps.DefaultMaxTokens))
	}
}

// MaxTokensFor 决定出站要写的输出上限。
//
// 抽成公共函数而不是留在 anthropic 的编码器里：判定「必填协议缺兜底值就
// 失败」这条路在当前四个协议上不可达（唯一必填的那个填了值），留在编码器
// 里就没有任何测试能走到它，等于一段没人验证过的保护。放在这里可以直接
// 用构造出来的 Capabilities 测。
//
// 返回的 ok 为假表示本协议可以省略该字段。
func MaxTokensFor(want int, caps Capabilities) (n int, ok bool, err error) {
	if want > 0 {
		// 客户端给了就原样用，不设下限：它要一个极短回答是它的事，
		// 静默抬高是无痕改写客户端的意图。
		return want, true, nil
	}
	if !caps.RequiresMaxTokens {
		return 0, false, nil
	}
	if caps.DefaultMaxTokens <= 0 {
		return 0, false, fmt.Errorf("max_tokens is required by this protocol but no default is configured")
	}
	return caps.DefaultMaxTokens, true, nil
}

// DroppedCandidatesNote 是上游回了多路候选而中立表示只装得下一路的说明。
//
// 带上实际路数：客户端为全部候选付了 token 费用（上游的 usage 是按全部
// 候选算的），只说「丢了些」看不出多付了多少。
//
// 一个流恒报一条：调用方须累计最大候选索引后只在收尾时调用本函数，
// 逐帧生成会让每帧的数字不同、去重失效。
func DroppedCandidatesNote(extra int) string {
	return fmt.Sprintf("dropped %d extra response candidate(s): the neutral representation holds one", extra)
}

// maxFinishDetail 是上游收尾原因原文的保留字节数。
// 说明会落库进流水，而原文长度不受本服务控制。
const maxFinishDetail = 200

// FinishDetailNote 是上游随 finish reason 附的人类可读原因被丢弃的说明。
//
// 原文必须带上：这条说明的全部价值就在原文里（比如具体触发了哪条安全
// 策略），砍掉只剩「有个细节丢了」等于没说。
func FinishDetailNote(detail string) string {
	if len(detail) > maxFinishDetail {
		detail = textsafe.Truncate(detail, maxFinishDetail) + "..."
	}
	return "dropped the upstream finish detail: " + detail
}

// DroppedServiceTierNote 是上游回了执行档位而入站协议无处安放的说明。
func DroppedServiceTierNote(name string) string {
	return "dropped service_tier from the response (" + name + " has no such field)"
}

// MediaOutputDropNote 是模型产出附件丢失的说明：图片与非图片附件分开计数，
// 合成一个数字会让排障时分不清丢的是哪一类——两者在源协议里是不同块型，
// 处置路径也不同。
//
// 两类调用方共用：助手回合没有任何附件形态的编码器（chat_completions /
// responses 两族），流式与非流式两条路径。
//
// 措辞刻意不断言「目标协议没有附件槽位」：只陈述本服务的转换带不过去。
// 文件名与 base64 本体属会话内容，不进说明。
func MediaOutputDropNote(images, files int) string {
	var subject string
	switch {
	case images > 0 && files > 0:
		subject = fmt.Sprintf("%d image(s) and %d non-image attachment(s)", images, files)
	case images > 0:
		subject = fmt.Sprintf("%d image(s)", images)
	default:
		subject = fmt.Sprintf("%d non-image attachment(s)", files)
	}
	return "dropped " + subject +
		" from the model output: this protocol's conversion has no way to carry them in an assistant turn, so the receiving side sees only the text the model produced"
}

// CountResponseMedia 数出响应里模型产出的附件块：图片单列，音频/文档/文件
// 合列——与 MediaOutputDropNote 的两类计数一一对应。
func CountResponseMedia(resp *ir.Response) (images, files int) {
	if resp == nil {
		return 0, 0
	}
	for _, b := range resp.Content {
		switch b.Type {
		case ir.BlockImage:
			images++
		case ir.BlockAudio, ir.BlockDocument, ir.BlockFile:
			files++
		}
	}
	return images, files
}

// CountResponseServerTools 数出响应里的托管工具块：调用与结果分开计数，
// 与 ServerToolDropNote 的两个入参一一对应。
func CountResponseServerTools(resp *ir.Response) (calls, results int) {
	if resp == nil {
		return 0, 0
	}
	for _, b := range resp.Content {
		switch b.Type {
		case ir.BlockServerToolUse:
			calls++
		case ir.BlockWebSearchToolResult:
			results++
		}
	}
	return calls, results
}

// CountResponseNonPortableCitations 数出响应里外族标注槽位装不下的引用条数
// （用于非流式有损诊断）。与 CountResponseServerTools 同一路数：编码器
// 逐条跳过之前先数出来，跳过才不是静默的。
func CountResponseNonPortableCitations(resp *ir.Response) int {
	if resp == nil {
		return 0
	}
	n := 0
	for _, b := range resp.Content {
		n += CountNonPortableCitations(b.Citations)
	}
	return n
}

// CountNonPortableCitations 统计一批引用里带不出本族的条数。
// Portable 判据见 ir.Citation：外族槽位以 URL 为来源身份。
func CountNonPortableCitations(cs []ir.Citation) int {
	n := 0
	for _, c := range cs {
		if !c.Portable() {
			n++
		}
	}
	return n
}

// CitationDropNote 文档类引用丢失注记：非流式扫描、流式编码器与请求侧诊断
// 三条通道共用同一措辞，按说明检索流水的人不会把同一件事当成多种故障。
// 引用的正文与文档标题属会话内容，不进注记。
func CitationDropNote(n int) string {
	return fmt.Sprintf(
		"dropped %d document citation(s): this protocol identifies an annotation source by URL, and these citations point at a document index with page/block/character offsets instead, so the client cannot see which passage was cited", n)
}

// countRequestServerTools 数出请求历史里的托管工具块。计数刻意分开：
// 两个数字对称时「接反」这类错误在夹具上看不出来。
func countRequestServerTools(req *ir.Request) (calls, results int) {
	count := func(blocks []ir.Block) {
		for _, b := range blocks {
			switch b.Type {
			case ir.BlockServerToolUse:
				calls++
			case ir.BlockWebSearchToolResult:
				results++
			}
		}
	}
	count(req.System)
	for _, m := range req.Messages {
		count(m.Content)
	}
	return calls, results
}

// ServerToolDropNote 服务端托管工具块丢失注记。server_tool_use 与
// web_search_tool_result 是**有 IR 块型**的，三个外族编码器都没有为它们
// 输出任何对应形态：整块消失且不计数时，接收端既看不到网关代执行了哪次
// 托管搜索，也拿不到搜回来的页面。calls / results 分别是两种块型的条数，
// 只渲染非零的那部分。搜索结果的标题、URL 与摘要属会话内容，不进注记。
//
// 措辞刻意不断言「目标协议没有槽位」：Responses 官方确有 web_search_call
// 输出项，只是本服务的转换没有实现映射。说的是转换做了什么，不是协议
// 没有什么。也刻意不写「客户端」：这条注记同时用于响应侧（受众是客户端）
// 与请求侧诊断（受众是上游模型），用「接收端」才对两个方向都成立。
//
// 三条路径共用：流式编码器 Notes()、非流式 EncodeResponseLossy、
// 请求侧 DescribeLossy。措辞只此一份，按说明检索流水的人不会把同一件事
// 当成多种故障。
func ServerToolDropNote(calls, results int) string {
	var subject string
	switch {
	case calls > 0 && results > 0:
		subject = fmt.Sprintf("%d server-side tool call(s) and %d web search result block(s)", calls, results)
	case calls > 0:
		subject = fmt.Sprintf("%d server-side tool call(s)", calls)
	default:
		subject = fmt.Sprintf("%d web search result block(s)", results)
	}
	return "dropped " + subject +
		": this protocol's conversion emits no counterpart for Anthropic's hosted-tool blocks, so the receiving side sees neither which hosted search ran nor which pages it returned, and cannot replay either in a later turn"
}

// toolErrorPrefix 是工具结果失败态在无原生标记的协议上的表达。
//
// 方括号形态在工具输出里罕见，不易与工具自己打的内容混淆。
const toolErrorPrefix = "[tool error] "

// PrefixToolResultError 在目标协议没有失败标记时给工具结果前置一个标记块。
//
// caps 承载得了就原样返回内容：anthropic 与 gemini 有原生字段，加前缀等于
// 把同一件事说两遍，其中一遍还混在工具的真实输出里。
//
// 返回新切片、不改入参：调用方的 IR 要留着换目标重试。
//
// 出处只有这一个：两个调用点若各写一份措辞，改了一处忘另一处的症状是
// 「换个目标协议，同一个失败的前缀文本不一样」，而下游按文本匹配的
// 客户端会只认出其中一种。
func PrefixToolResultError(result *ir.ToolResult, caps Capabilities) []ir.Block {
	if result == nil {
		return nil
	}
	if !result.IsError || caps.ToolResultError {
		return result.Content
	}
	out := make([]ir.Block, 0, len(result.Content)+1)
	out = append(out, ir.Block{Type: ir.BlockText, Text: toolErrorPrefix})
	return append(out, result.Content...)
}

// ToolErrorPrefix 供需要拼字符串的调用点取用同一份措辞。
func ToolErrorPrefix() string { return toolErrorPrefix }

// AdoptToolResultError 在入站解码时把前缀还原成失败标记位。
//
// 没有原生失败字段的协议（chat_completions、responses）里，失败态是我们
// 自己上一轮用 PrefixToolResultError 写进正文的。客户端把整段历史回传后，
// 若不认这个前缀，标记位读作 false：再路由到 anthropic 或 gemini 时模型被
// 告知工具调用成功，而正文写着 [tool error] connection refused。多跳还会
// 把前缀叠成 [tool error] [tool error] …。
//
// 剥掉前缀而不是只置位：留着它等于把同一件事说两遍，且下一跳若又落到无
// 原生字段的协议，PrefixToolResultError 会再加一层。
//
// 只认块首那一处：正文中间出现同样的字面量是工具自己打的内容，
// 改写它会篡改工具输出。
func AdoptToolResultError(blocks []ir.Block) ([]ir.Block, bool) {
	if len(blocks) == 0 || blocks[0].Type != ir.BlockText {
		return blocks, false
	}
	if !strings.HasPrefix(blocks[0].Text, toolErrorPrefix) {
		return blocks, false
	}
	out := make([]ir.Block, len(blocks))
	copy(out, blocks)
	out[0].Text = strings.TrimPrefix(out[0].Text, toolErrorPrefix)
	// 前缀独占一块时（PrefixToolResultError 写出的正是这个形态）整块去掉，
	// 留一个空文本块会让下游协议多出一个无内容的 part。
	if out[0].Text == "" {
		out = out[1:]
	}
	return out, true
}

// hasFailedToolResult 判断请求里是否带着失败的工具结果。
func hasFailedToolResult(req *ir.Request) bool {
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockToolResult && b.ToolResult != nil && b.ToolResult.IsError {
				return true
			}
		}
	}
	return false
}

// hasImageDetail 判断请求里是否有客户端指定了 detail 的图片块。
func hasImageDetail(req *ir.Request) bool {
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockImage && b.Media != nil && b.Media.Detail != "" {
				return true
			}
		}
	}
	return false
}
