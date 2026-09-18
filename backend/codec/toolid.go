package codec

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// SynthIDPrefix 标记由本服务合成的工具调用 id。
//
// 上游不给 id 时必须合成一个：anthropic / chat_completions / responses 都要
// 调用 id 才能把工具结果回指到调用。但合成的 id 出站时要与上游原生 id 区别
// 对待（见 OmitSynthToolID 的用处），而出站侧只拿到 id 文本、拿不到「这个 id
// 是谁给的」这一信息，所以来源必须编码进文本本身。
//
// 不用通用的 call_ 前缀：那正是 OpenAI 原生 id 的实际前缀，
// 用它做判定会把上游真 id 误判成自己合成的，进而错误地省略掉。
const SynthIDPrefix = "msa_synth_"

// SynthToolID 合成工具调用 id。三处解码路径共用此函数：
// 各自拼字符串会让前缀判定在漏改的那一处静默失效。
func SynthToolID(name string, seq int) string {
	if name == "" {
		return fmt.Sprintf("%s%d", SynthIDPrefix, seq)
	}
	return fmt.Sprintf("%s%s_%d", SynthIDPrefix, name, seq)
}

// IsSynthToolID 判断 id 是否由本服务合成。
func IsSynthToolID(id string) bool {
	return strings.HasPrefix(id, SynthIDPrefix)
}

// toolIDHashLen 是收敛长 id 时附加的哈希位数，与 ir 的工具名治理同宽。
const toolIDHashLen = 4

// shortenToolID 把超长 id 收敛到 max 字节以内。
//
// 保留前缀而非只留哈希：前缀承载「是否自己合成」的判定，丢了它
// 出站侧就无从决定该不该省略。salt 用于撞车后重取，同一 salt 结果稳定。
func shortenToolID(id string, max, salt int) string {
	if max <= 0 || len(id) <= max {
		return id
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s#%d", id, salt)))
	tail := "_" + hex.EncodeToString(sum[:])[:toolIDHashLen]
	if max <= len(tail) {
		// 上限比哈希尾还短：只能给出哈希尾的截断，唯一性尽力而为。
		return tail[len(tail)-max:]
	}
	return id[:max-len(tail)] + tail
}
