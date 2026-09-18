package codec

import (
	"fmt"
	"sort"
	"strings"

	"github.com/aceaura/model-surge-agent/backend/ir"
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

	if len(req.Tools) > 0 && !caps.Tools {
		note("tools", "no tool calling")
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
	describeParamsLossy(req, caps, note, filled)

	describeBlocksLossy(req.System, name, caps, note)
	for _, m := range req.Messages {
		describeBlocksLossy(m.Content, name, caps, note)
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
			if !caps.AcceptsMedia(SniffMediaType(b.Media)) {
				note(string(b.Type)+" blocks", "unsupported media type, downgraded to text")
			}

		case b.Type == ir.BlockToolUse, b.Type == ir.BlockToolResult:
			if !caps.Tools {
				note("tool blocks", "no tool calling")
			}
			if b.ToolResult != nil {
				describeBlocksLossy(b.ToolResult.Content, name, caps, note)
			}
		}
	}
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
func describeParamsLossy(req *ir.Request, caps Capabilities, note, filled func(field, why string)) {
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
	if req.Candidates != nil && !caps.Candidates {
		// 措辞要说清后果：客户端按数组取第二个候选会越界，
		// 笼统的 dropped n 读不出这一点。
		note("n", "no multi-candidate parameter, only one candidate will be returned")
	}
	if !caps.LogProbs {
		if req.LogProbs != nil {
			note("logprobs", "no log probability parameter")
		}
		if req.TopLogProbs != nil {
			note("top_logprobs", "no log probability parameter")
		}
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
