package httpapi

import (
	"fmt"
	"strings"
	"testing"
)

// 未撞号时键的形态不变：不得因为加了这道检查就让所有既有记录换形态。
func TestClaimPassesThroughFirstTime(t *testing.T) {
	g := newIDGuard()
	got, collided := g.claim("req-from-client")
	if got != "req-from-client" {
		t.Errorf("键被改了：got %q", got)
	}
	if collided {
		t.Error("首次登记不应报撞号")
	}
}

// 撞号时必须换键，否则第二个请求会覆盖第一个的整条流水。
func TestClaimReplacesKeyOnCollision(t *testing.T) {
	g := newIDGuard()
	first, _ := g.claim("dup")
	second, collided := g.claim("dup")
	if !collided {
		t.Fatal("撞号未被检出")
	}
	if second == first {
		t.Fatalf("撞号后仍用同一个键：%q", second)
	}
	if second == "dup" {
		t.Fatalf("撞号后仍用客户端的值：%q", second)
	}
	if !strings.HasPrefix(second, "req_") {
		t.Errorf("生成的键前缀不对：%q", second)
	}
}

// 同一个 id 撞两次要得到两个互不相同的键：前缀化方案在这里会失效。
func TestClaimGeneratesDistinctKeysForRepeatedCollisions(t *testing.T) {
	g := newIDGuard()
	g.claim("dup")
	a, _ := g.claim("dup")
	b, _ := g.claim("dup")
	if a == b {
		t.Fatalf("两次撞号给了同一个键：%q", a)
	}
}

// 生成的键本身也要进环，否则它自己会成为下一次撞号的漏洞。
func TestGeneratedKeyIsAlsoRemembered(t *testing.T) {
	g := newIDGuard()
	g.claim("dup")
	gen, _ := g.claim("dup")
	if _, ok := g.seen[gen]; !ok {
		t.Error("生成的键没进环")
	}
}

// 环必须有界：无界集合等于给客户端一个用请求 id 撑爆内存的口子。
func TestGuardEvictsOldestBeyondCapacity(t *testing.T) {
	g := newIDGuard()
	for i := range idGuardCap + 10 {
		g.claim(fmt.Sprintf("id-%d", i))
	}
	if got := len(g.seen); got != idGuardCap {
		t.Errorf("环内 = %d 项，want %d", got, idGuardCap)
	}
	if g.order.Len() != idGuardCap {
		t.Errorf("链表 = %d 项，want %d", g.order.Len(), idGuardCap)
	}
	// 最老的已被淘汰，所以它再来不算撞号。
	if _, collided := g.claim("id-0"); collided {
		t.Error("已淘汰的 id 仍被判撞号")
	}
	// 最新的还在环里。
	if _, collided := g.claim(fmt.Sprintf("id-%d", idGuardCap+9)); !collided {
		t.Error("环内的 id 未被判撞号")
	}
}

// 不同 id 互不干扰。
func TestClaimDistinctIDsNeverCollide(t *testing.T) {
	g := newIDGuard()
	for i := range 100 {
		id := fmt.Sprintf("id-%d", i)
		got, collided := g.claim(id)
		if collided || got != id {
			t.Fatalf("id %q 被误判：got %q collided=%v", id, got, collided)
		}
	}
}
