package httpapi_test

import (
	"strings"
	"testing"
)

const dupID = "dup-client-id"

// 撞号时两条流水都要留下：ON CONFLICT DO UPDATE 让第二条覆盖第一条，
// 而那是静默的。
func TestCollidingClientIDsKeepBothRecords(t *testing.T) {
	// 两个 dispatch 答案：第二次请求也必须真走到数据面。只给一个的话
	// 它在选目标那步就 404 了，撞号之后的代码一行都不执行。
	f := newFixtureSteps(t, 2, nil, nil)
	body := `{"model":"user-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	hdr := map[string]string{"X-Request-Id": dupID}

	f.post(t, "/v1/messages", body, hdr)
	f.post(t, "/v1/messages", body, hdr)

	recs := f.records.all()
	if len(recs) != 2 {
		t.Fatalf("流水 = %d 条，want 2", len(recs))
	}
	for i, r := range recs {
		if r.Outcome != "normal" {
			t.Fatalf("第 %d 条不是成功路径（outcome=%q）：撞号之后的代码没被执行",
				i+1, r.Outcome)
		}
	}
	// 主键两两不同：相同就意味着后写的那条覆盖了前面那条。
	seen := map[string]bool{}
	for _, r := range recs {
		if seen[r.RequestID] {
			t.Fatalf("流水主键 %q 出现两次，后写的会覆盖前面那条", r.RequestID)
		}
		seen[r.RequestID] = true
	}
	// 第一条未撞号，键形态不变。
	if recs[0].RequestID != dupID {
		t.Errorf("首条流水的键被改了：got %q, want %q", recs[0].RequestID, dupID)
	}
	// 其余都是撞号后生成的。
	for _, r := range recs[1:] {
		if !strings.HasPrefix(r.RequestID, "req_") {
			t.Errorf("撞号那条的键前缀不对：%q", r.RequestID)
		}
	}
}

// 客户端可见的回显仍是客户端给的那个值：改掉它会让客户端对不上自己的日志。
func TestCollidingClientIDStillEchoesClientValue(t *testing.T) {
	// 同上要两个答案：撞号那次必须真走数据面那行回显，否则回显来自
	// reject 路径，测的就不是这里要测的代码。
	f := newFixtureSteps(t, 2, nil, nil)
	body := `{"model":"user-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	hdr := map[string]string{"X-Request-Id": dupID}

	f.post(t, "/v1/messages", body, hdr)
	w := f.post(t, "/v1/messages", body, hdr)

	if w.Code != 200 {
		t.Fatalf("撞号那次状态码 = %d，没走到数据面：%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Request-Id"); got != dupID {
		t.Errorf("回显 = %q, want %q（客户端要能对上自己的日志）", got, dupID)
	}
	// 而落库用的是换过的键：两者必须确实不同，否则上面那个断言
	// 在「回显与落库仍是同一个值」的实现下也会通过。
	recs := f.records.all()
	if len(recs) != 2 {
		t.Fatalf("流水 = %d 条，want 2", len(recs))
	}
	if recs[1].RequestID == dupID {
		t.Errorf("撞号那条的落库键仍是客户端给的值 %q：会覆盖前一条", dupID)
	}
}

// 不撞号的请求回显与落库用同一个键：这是绝大多数请求的形态，不能变。
func TestUniqueClientIDUsesSameKeyEverywhere(t *testing.T) {
	f := newFixture(t)
	body := `{"model":"user-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	w := f.post(t, "/v1/messages", body, map[string]string{"X-Request-Id": "solo-id"})

	if got := w.Header().Get("X-Request-Id"); got != "solo-id" {
		t.Errorf("回显 = %q, want solo-id", got)
	}
	if got := f.records.one(t).RequestID; got != "solo-id" {
		t.Errorf("流水键 = %q, want solo-id", got)
	}
}

// 受理面拒绝这条路径也落流水，撞号一样会覆盖。
func TestRejectPathAlsoGuardsAgainstCollision(t *testing.T) {
	f := newFixture(t)
	// 缺 model：走 reject 那条路径。
	bad := `{"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	hdr := map[string]string{"X-Request-Id": dupID}

	f.post(t, "/v1/messages", bad, hdr)
	w := f.post(t, "/v1/messages", bad, hdr)

	recs := f.records.all()
	if len(recs) != 2 {
		t.Fatalf("流水 = %d 条，want 2", len(recs))
	}
	if recs[0].RequestID == recs[1].RequestID {
		t.Fatalf("两条拒绝流水同主键 %q", recs[0].RequestID)
	}
	if recs[0].RequestID != dupID {
		t.Errorf("首条拒绝流水的键被改了：%q", recs[0].RequestID)
	}
	if got := w.Header().Get("X-Request-Id"); got != dupID {
		t.Errorf("拒绝路径的回显 = %q, want %q", got, dupID)
	}
}
