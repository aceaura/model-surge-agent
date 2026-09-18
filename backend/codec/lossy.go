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
var foreignSigPrefixes = []string{
	// OpenAI/Codex 系的加密推理载荷。
	"gAAAA",
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

	if len(req.Tools) > 0 && !caps.Tools {
		note("tools", "no tool calling")
	}
	if req.TopK != nil && !caps.TopK {
		note("top_k", "no top_k parameter")
	}
	if len(req.StopSequences) > 0 && !caps.StopSequences {
		note("stop_sequences", "no stop sequence parameter")
	}
	if req.Thinking != nil && req.Thinking.Enabled && !caps.Thinking {
		note("thinking", "no reasoning mode")
	}

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
	for _, p := range foreignSigPrefixes {
		if strings.HasPrefix(t.Signature, p) {
			return "signature carries another vendor's ciphertext", true
		}
	}
	if t.SignatureFrom != name {
		return "signature is only valid within its own protocol family", true
	}
	return "", false
}

// ForeignSignature 判断签名是否属于别家协议，出站 codec 编码前用它决定是否剥离。
//
// 与 DescribeLossy 共用一套判定：诊断说丢了而编码器实际发了出去，
// 比不诊断更糟——排查的人会照着诊断去找一个不存在的原因。
func ForeignSignature(t *ir.Thinking, name string) bool {
	if t == nil || t.Signature == "" {
		return false
	}
	for _, p := range foreignSigPrefixes {
		if strings.HasPrefix(t.Signature, p) {
			return true
		}
	}
	return t.SignatureFrom != name
}
