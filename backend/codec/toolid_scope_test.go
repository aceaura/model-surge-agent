package codec_test

import (
	"strings"
	"testing"

	"github.com/aceaura/model-surge-agent/backend/codec"
)

// 同一次响应内 scope 不变，同名同序号必须给出同一个 id：
// 解码期间一次调用会多次问到 id（宣告、续写、闭合），每次不同就拆成多个块。
func TestSameScopeAndSeqIsStable(t *testing.T) {
	a := codec.SynthToolID("resp-1", "grep", 2)
	b := codec.SynthToolID("resp-1", "grep", 2)
	if a != b {
		t.Fatalf("同 scope 同序号给出两个 id：%q 与 %q", a, b)
	}
}

// 跨轮不撞是本轮的核心：响应 id 不同即 id 不同，
// 否则客户端把两轮都回传进历史后配对会错位到另一轮的调用上。
func TestDifferentScopesDoNotCollide(t *testing.T) {
	a := codec.SynthToolID("resp-1", "grep", 1)
	b := codec.SynthToolID("resp-2", "grep", 1)
	if a == b {
		t.Fatalf("两次响应的同名同序号调用撞成同一个 id %q", a)
	}
}

// 同一次响应内的并行调用也不得撞：scope 相同，只有序号能区分它们。
func TestSameScopeDifferentSeqDoNotCollide(t *testing.T) {
	seen := map[string]bool{}
	for i := 1; i <= 8; i++ {
		id := codec.SynthToolID("resp-1", "grep", i)
		if seen[id] {
			t.Fatalf("序号 %d 撞上了已有 id %q", i, id)
		}
		seen[id] = true
	}
}

// scope 缺失（兼容层网关常不给响应 id）时不得退回固定串：
// 固定串会让所有缺响应 id 的上游回到「只有序号」那个撞车状态。
func TestMissingScopeStillSeparatesTurns(t *testing.T) {
	a := codec.SynthToolID("", "grep", 1)
	b := codec.SynthToolID("", "grep", 1)
	if a == b {
		t.Fatalf("scope 缺失时两轮拿到同一个 id %q，跨轮撞车照旧", a)
	}
	for _, id := range []string{a, b} {
		if !codec.IsSynthToolID(id) {
			t.Errorf("%q 判不回合成 id", id)
		}
	}
}

// 加了 scope 之后仍必须能判回合成、仍必须保留函数名：
// 前者决定出站要不要省略，后者是排查时唯一能看出「这是哪个调用」的线索。
func TestScopedIDKeepsPrefixAndName(t *testing.T) {
	id := codec.SynthToolID("resp-1", "grep", 1)
	if !codec.IsSynthToolID(id) {
		t.Errorf("IsSynthToolID(%q) = false", id)
	}
	if !strings.Contains(id, "grep") {
		t.Errorf("%q 未保留函数名", id)
	}
}

// scope 不得原样拼进 id：Gemini 的 responseId 很长，
// 拼进去会把工具 id 顶到各家长度上限附近，反而触发 id 收敛。
func TestLongScopeDoesNotInflateTheID(t *testing.T) {
	long := strings.Repeat("r", 400)
	id := codec.SynthToolID(long, "grep", 1)
	if strings.Contains(id, long) {
		t.Errorf("%q 原样拼进了上游响应 id", id)
	}
	if len(id) > 64 {
		t.Errorf("id 长 %d 字节（%q），超出各家上限的安全余量", len(id), id)
	}
}

// 空名 + 有 scope 也不得留下连续下划线：
// 上游先发 arguments 后发 name 时合成发生在 name 到达之前。
func TestScopedIDWithoutNameHasNoDoubleUnderscore(t *testing.T) {
	id := codec.SynthToolID("resp-1", "", 3)
	if strings.Contains(id, "__") {
		t.Errorf("%q 带空名留下的连续下划线", id)
	}
	if !codec.IsSynthToolID(id) {
		t.Errorf("IsSynthToolID(%q) = false", id)
	}
}
