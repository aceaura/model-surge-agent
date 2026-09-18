package codec

import (
	"crypto/rand"
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

// SynthToolID 合成工具调用 id。各解码路径共用此函数：
// 各自拼字符串会让前缀判定在漏改的那一处静默失效。
//
// scope 标识「哪一次响应」（用上游的响应 id），seq 是该次响应内的序号。
// 两者都必须参与：
//
// 只有 seq 时，第二轮的同名调用会拿到与第一轮一样的 id。客户端把两轮
// 都回传进历史，于是历史里出现同 id 的两次 tool_use——配对治理据此把
// 前一轮的结果当成「重复结果」降级成文本，把后一轮的结果绑到前一轮的
// 调用上，参数与结果就对错了，而这一切只留下一条看不出根因的说明。
//
// 只有 scope 时，同一次响应里的多个并行调用会互相撞。
func SynthToolID(scope, name string, seq int) string {
	tag := scopeTag(scope)
	if name == "" {
		return fmt.Sprintf("%s%s_%d", SynthIDPrefix, tag, seq)
	}
	return fmt.Sprintf("%s%s_%s_%d", SynthIDPrefix, tag, name, seq)
}

// scopeTagLen 是 scope 收敛后的十六进制位数。
//
// 6 位 = 16M 取值，而需要区分的只是「同一个会话里前后相邻的几轮」这个量级。
// 不原样拼上游的响应 id：Gemini 的 responseId 很长，拼进去会把工具 id
// 顶到各家的长度上限附近，反而触发 id 收敛。
const scopeTagLen = 6

// scopeTag 把 scope 收敛成定长十六进制。
//
// scope 为空（兼容层网关常不给响应 id）时给一个随机值而不是固定串：
// 固定串会让所有缺响应 id 的上游退回到「只有 seq」那个撞车状态，
// 而跨轮不撞正是本函数存在的理由。随机值不可复现，但同一次响应内
// scope 由调用方持有、不变，所以该次响应的各调用仍共享同一个 tag。
func scopeTag(scope string) string {
	if scope == "" {
		return randomHex(scopeTagLen)
	}
	sum := sha256.Sum256([]byte(scope))
	return hex.EncodeToString(sum[:])[:scopeTagLen]
}

// randomHex 给出 n 位十六进制随机串。取不到随机源时退回一个定值：
// 合成 id 本身不是安全边界，拿不到熵也不该让整次响应解不出来。
func randomHex(n int) string {
	buf := make([]byte, (n+1)/2)
	if _, err := rand.Read(buf); err != nil {
		return strings.Repeat("0", n)
	}
	return hex.EncodeToString(buf)[:n]
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
