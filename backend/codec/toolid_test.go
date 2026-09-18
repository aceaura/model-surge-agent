package codec_test

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
)

// 合成 id 必须能被判定回来：出站侧只拿到 id 文本，
// 判不出来源就无从决定该不该省略。
func TestSynthToolIDIsRecognizable(t *testing.T) {
	id := codec.SynthToolID("resp-1", "grep", 1)
	if !codec.IsSynthToolID(id) {
		t.Errorf("IsSynthToolID(%q) = false，合成 id 判不回来", id)
	}
	if !strings.Contains(id, "grep") {
		t.Errorf("%q 未保留函数名，排查时看不出这是哪个调用", id)
	}
}

// name 缺席时仍要给出可判定的 id：上游先发 arguments 后发 name 时
// 合成发生在 name 到达之前。
func TestSynthToolIDWithoutName(t *testing.T) {
	id := codec.SynthToolID("resp-1", "", 3)
	if !codec.IsSynthToolID(id) {
		t.Errorf("IsSynthToolID(%q) = false", id)
	}
	if strings.Contains(id, "__") {
		t.Errorf("%q 带空名留下的连续下划线", id)
	}
}

// 同一次转换内序号不同即 id 不同：撞号会让两次调用共用一个槽位，
// 客户端把两份入参串成一份非法 JSON。
func TestSynthToolIDIsUniquePerSeq(t *testing.T) {
	seen := map[string]bool{}
	for i := 1; i <= 8; i++ {
		id := codec.SynthToolID("resp-1", "grep", i)
		if seen[id] {
			t.Fatalf("序号 %d 产出重复 id %q", i, id)
		}
		seen[id] = true
	}
}

// 上游原生 id 一律不得被判成合成：误判的后果是把一个上游认得的 id
// 省略掉，上游从此对不上这次调用。call_ 是 OpenAI 原生前缀，
// 正是最容易被误判的那一类。
func TestUpstreamIDsAreNotMistakenForSynth(t *testing.T) {
	for _, id := range []string{
		"call_abc123",
		"toolu_01ABC",
		"fc_68f1",
		"",
		"msa_other",
	} {
		if codec.IsSynthToolID(id) {
			t.Errorf("IsSynthToolID(%q) = true，上游原生 id 被误判成合成", id)
		}
	}
}
