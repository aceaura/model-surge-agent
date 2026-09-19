package capture

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestParseModeAcceptsThreeStates(t *testing.T) {
	for raw, want := range map[string]Mode{
		"":       ModeOff,
		"off":    ModeOff,
		"errors": ModeErrors,
		"all":    ModeAll,
	} {
		got, err := ParseMode(raw)
		if err != nil {
			t.Fatalf("ParseMode(%q): %v", raw, err)
		}
		if got != want {
			t.Errorf("ParseMode(%q) = %q, want %q", raw, got, want)
		}
	}
}

// 非法值必须报错而不是静默回落到 off：静默的后果是运维以为捕获开着，
// 等出了故障才发现什么都没留。
func TestParseModeRejectsUnknown(t *testing.T) {
	for _, raw := range []string{"ERRORS", "on", "true", "1", "Off"} {
		if _, err := ParseMode(raw); err == nil {
			t.Errorf("ParseMode(%q) 静默通过了，非法模式必须在启动时报错", raw)
		}
	}
}

func TestOffModeBeginsNothing(t *testing.T) {
	st := New(Options{Mode: ModeOff})
	if sn := st.Begin("r1"); sn != nil {
		t.Fatalf("off 档 Begin 返回了非 nil 会话：%#v", sn)
	}
	// nil 会话上的每个方法都必须安全。
	var sn *Session
	sn.Add(ClientRequest, []byte("x"))
	sn.Set(UpstreamRequest, []byte("y"))
	sn.Finish(true)
	if w := sn.Writer(ClientResponse); w != io.Discard {
		t.Errorf("nil 会话的 Writer 应回 io.Discard，实际 %#v", w)
	}
	if len(st.List()) != 0 {
		t.Errorf("off 档不该留下任何捕获")
	}
}

func TestNilStoreIsSafe(t *testing.T) {
	var st *Store
	if st.Begin("r1") != nil {
		t.Error("nil Store 的 Begin 应回 nil")
	}
	if st.Mode() != ModeOff {
		t.Errorf("nil Store 的 Mode 应为 off，实际 %q", st.Mode())
	}
	if st.List() != nil {
		t.Error("nil Store 的 List 应回 nil")
	}
	if _, ok := st.Get("r1"); ok {
		t.Error("nil Store 的 Get 应回 false")
	}
}

func TestAllModeKeepsSuccessAndFailure(t *testing.T) {
	st := New(Options{Mode: ModeAll})
	st.Begin("ok").Finish(false)
	st.Begin("bad").Finish(true)
	if got := len(st.List()); got != 2 {
		t.Fatalf("all 档应留下 2 条，实际 %d", got)
	}
}

func TestErrorsModeKeepsOnlyFailure(t *testing.T) {
	st := New(Options{Mode: ModeErrors})
	okSess := st.Begin("ok")
	okSess.Add(ClientRequest, []byte(`{"model":"m"}`))
	okSess.Finish(false)

	badSess := st.Begin("bad")
	badSess.Add(ClientRequest, []byte(`{"model":"m"}`))
	badSess.Finish(true)

	list := st.List()
	if len(list) != 1 {
		t.Fatalf("errors 档应只留失败那条，实际留了 %d 条", len(list))
	}
	if list[0].RequestID != "bad" {
		t.Errorf("留下的是 %q，应是失败的 bad", list[0].RequestID)
	}
	if _, ok := st.Get("ok"); ok {
		t.Error("成功的请求被留下了：errors 档必须丢弃它")
	}
	// 丢弃要真的释放缓冲，不能只是不进 store。
	if n := len(okSess.bodies[ClientRequest].buf); n != 0 {
		t.Errorf("丢弃后缓冲仍占 %d 字节，应被清空以便回收", n)
	}
}

func TestFourBodiesAreIndependent(t *testing.T) {
	st := New(Options{Mode: ModeAll})
	sn := st.Begin("r1")
	// 刻意只填三体：缺一体不能影响另外三体。
	sn.Add(ClientRequest, []byte("cli-req"))
	sn.Set(UpstreamRequest, []byte("up-req"))
	sn.Add(ClientResponse, []byte("cli-resp"))
	sn.Finish(true)

	snap, ok := st.Get("r1")
	if !ok {
		t.Fatal("捕获没留下")
	}
	for kind, want := range map[Kind]string{
		ClientRequest:    "cli-req",
		UpstreamRequest:  "up-req",
		UpstreamResponse: "",
		ClientResponse:   "cli-resp",
	} {
		if got := string(snap.Bodies[kind].Bytes); got != want {
			t.Errorf("kind %d = %q, want %q", kind, got, want)
		}
	}
}

// Add 累加、Set 覆盖，两种语义刻意不同：上游字节是一个流的连续片段，
// 而出站 wire body 在换目标重试时要的是最终发出去的那一份。
func TestAddAccumulatesAndSetOverwrites(t *testing.T) {
	st := New(Options{Mode: ModeAll})
	sn := st.Begin("r1")
	sn.Add(UpstreamResponse, []byte("frame1;"))
	sn.Add(UpstreamResponse, []byte("frame2;"))
	sn.Set(UpstreamRequest, []byte(`{"attempt":1}`))
	sn.Set(UpstreamRequest, []byte(`{"attempt":2}`))
	sn.Finish(true)

	snap, _ := st.Get("r1")
	if got := string(snap.Bodies[UpstreamResponse].Bytes); got != "frame1;frame2;" {
		t.Errorf("上游字节应累加，实际 %q", got)
	}
	if got := string(snap.Bodies[UpstreamRequest].Bytes); got != `{"attempt":2}` {
		t.Errorf("出站 body 应被最后一次覆盖，实际 %q —— "+
			"累加会把一份没被采用的 body 拼在前面，读的人分不开两份", got)
	}
}

// Set 也要清掉上一次的截断标记，否则一次大 body 之后的小 body 会一直挂着
// 「被截断」，读的人会以为自己看的是残缺的。
func TestSetClearsPreviousTruncation(t *testing.T) {
	st := New(Options{Mode: ModeAll, MaxBody: 8})
	sn := st.Begin("r1")
	sn.Set(UpstreamRequest, []byte("0123456789"))
	if !sn.bodies[UpstreamRequest].truncated {
		t.Fatal("超限的 Set 没标截断")
	}
	sn.Set(UpstreamRequest, []byte("ab"))
	b := sn.bodies[UpstreamRequest]
	if b.truncated || b.dropped != 0 {
		t.Errorf("Set 没清掉上一次的截断标记：truncated=%v dropped=%d", b.truncated, b.dropped)
	}
}

// 截断保留**前段**：请求体的诊断价值在头部的模型名与参数上，
// 响应流的头部则是 message_start——只留尾部会把它挤掉。
func TestTruncationKeepsFrontAndCountsDropped(t *testing.T) {
	st := New(Options{Mode: ModeAll, MaxBody: 10})
	sn := st.Begin("r1")
	sn.Add(ClientRequest, []byte("0123456"))
	sn.Add(ClientRequest, []byte("789ABCDE"))
	sn.Add(ClientRequest, []byte("FFFF"))
	sn.Finish(true)

	snap, _ := st.Get("r1")
	b := snap.Bodies[ClientRequest]
	if string(b.Bytes) != "0123456789" {
		t.Errorf("截断应留前 10 字节，实际 %q（留尾部会把 message_start 挤掉）", b.Bytes)
	}
	if !b.Truncated {
		t.Error("超限了却没标 truncated")
	}
	// 7+8+4=19，留 10，丢 9。
	if b.Dropped != 9 {
		t.Errorf("dropped = %d, want 9 —— 少看了多少必须能看出来", b.Dropped)
	}
}

func TestExactLimitIsNotTruncated(t *testing.T) {
	st := New(Options{Mode: ModeAll, MaxBody: 4})
	sn := st.Begin("r1")
	sn.Add(ClientRequest, []byte("abcd"))
	sn.Finish(true)
	snap, _ := st.Get("r1")
	if snap.Bodies[ClientRequest].Truncated {
		t.Error("正好等于上限被误判为截断")
	}
	if got := string(snap.Bodies[ClientRequest].Bytes); got != "abcd" {
		t.Errorf("正好等于上限时内容 = %q, want abcd", got)
	}
}

// 填满之后再写走的是「一点余量都没有」那条分支，与「这一次写超了」不是
// 同一条。不单独测的话那条分支漏标截断也看不出来。
func TestWritingAfterExactFillMarksTruncation(t *testing.T) {
	st := New(Options{Mode: ModeAll, MaxBody: 4})
	sn := st.Begin("r1")
	sn.Add(ClientRequest, []byte("abcd"))
	sn.Add(ClientRequest, []byte("efg"))
	sn.Finish(true)
	snap, _ := st.Get("r1")
	b := snap.Bodies[ClientRequest]
	if !b.Truncated {
		t.Error("已满之后再写没标截断，读的人会以为自己看的是完整的")
	}
	if b.Dropped != 3 {
		t.Errorf("dropped = %d, want 3", b.Dropped)
	}
	if string(b.Bytes) != "abcd" {
		t.Errorf("内容被改了：%q", b.Bytes)
	}
}

func TestEvictsOldestBeyondMaxEntries(t *testing.T) {
	st := New(Options{Mode: ModeAll, MaxEntries: 3})
	for i := range 5 {
		st.Begin(fmt.Sprintf("r%d", i)).Finish(true)
	}
	list := st.List()
	if len(list) != 3 {
		t.Fatalf("条数上限 3，实际留了 %d", len(list))
	}
	// 最新在前。
	for i, want := range []string{"r4", "r3", "r2"} {
		if list[i].RequestID != want {
			t.Errorf("list[%d] = %q, want %q（最新在前，最旧被淘汰）", i, list[i].RequestID, want)
		}
	}
	if _, ok := st.Get("r0"); ok {
		t.Error("最旧的 r0 没被淘汰")
	}
}

// 同一 requestID 重复写入不能把序列表撑大，否则重试场景下条数上限会失效。
func TestRewritingSameIDDoesNotGrowOrder(t *testing.T) {
	st := New(Options{Mode: ModeAll, MaxEntries: 2})
	st.Begin("r1").Finish(true)
	st.Begin("r1").Finish(true)
	st.Begin("r2").Finish(true)
	if got := len(st.List()); got != 2 {
		t.Fatalf("应有 2 条，实际 %d", got)
	}
	if _, ok := st.Get("r1"); !ok {
		t.Error("r1 被误淘汰了：重复写入不该占两个位置")
	}
}

func TestDefaultsAppliedForZeroOptions(t *testing.T) {
	st := New(Options{Mode: ModeAll})
	if st.opts.MaxBody != DefaultMaxBody {
		t.Errorf("MaxBody 零值没取默认：%d", st.opts.MaxBody)
	}
	if st.opts.MaxEntries != DefaultMaxEntries {
		t.Errorf("MaxEntries 零值没取默认：%d", st.opts.MaxEntries)
	}
	if New(Options{}).Mode() != ModeOff {
		t.Error("Mode 零值应为 off")
	}
}

// 并发请求必须各自独立。kiro-gateway 的实现是单例加每请求清空一个共享目录，
// 并发下互相擦掉证据——这个测试就是钉住「不能那样」。
func TestConcurrentSessionsAreIsolated(t *testing.T) {
	st := New(Options{Mode: ModeAll, MaxEntries: 64})
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("r%02d", i)
			sn := st.Begin(id)
			for range 4 {
				sn.Add(ClientRequest, []byte(id))
			}
			sn.Finish(true)
		}()
	}
	wg.Wait()

	if got := len(st.List()); got != 32 {
		t.Fatalf("32 个并发请求应各留一条，实际 %d", got)
	}
	for i := range 32 {
		id := fmt.Sprintf("r%02d", i)
		snap, ok := st.Get(id)
		if !ok {
			t.Fatalf("%s 的捕获不见了", id)
		}
		if want := strings.Repeat(id, 4); string(snap.Bodies[ClientRequest].Bytes) != want {
			t.Errorf("%s 的内容被别的请求污染了：%q", id, snap.Bodies[ClientRequest].Bytes)
		}
	}
}

// Writer 是给 io.TeeReader 用的，写失败不能中断被排查的数据流。
func TestWriterNeverFailsAndFeedsBody(t *testing.T) {
	st := New(Options{Mode: ModeAll, MaxBody: 4})
	sn := st.Begin("r1")
	w := sn.Writer(UpstreamResponse)
	n, err := w.Write([]byte("0123456789"))
	if err != nil || n != 10 {
		t.Fatalf("Write = (%d, %v)，捕获超限不能表达成写失败，"+
			"否则 TeeReader 会中断真正的数据流", n, err)
	}
	sn.Finish(true)
	snap, _ := st.Get("r1")
	if string(snap.Bodies[UpstreamResponse].Bytes) != "0123" {
		t.Errorf("Writer 没把字节喂进去：%q", snap.Bodies[UpstreamResponse].Bytes)
	}
}

// 捕获必须复制字节：出站 wire body 与上游读缓冲都会被复用，
// 持有引用会让捕获里的内容在之后被悄悄改写。
func TestCaptureCopiesBytes(t *testing.T) {
	st := New(Options{Mode: ModeAll})
	sn := st.Begin("r1")
	buf := []byte("original")
	sn.Add(ClientRequest, buf)
	sn.Set(UpstreamRequest, buf)
	copy(buf, "MUTATED!")
	sn.Finish(true)

	snap, _ := st.Get("r1")
	if got := string(snap.Bodies[ClientRequest].Bytes); got != "original" {
		t.Errorf("Add 持有了调用方的切片：%q", got)
	}
	if got := string(snap.Bodies[UpstreamRequest].Bytes); got != "original" {
		t.Errorf("Set 持有了调用方的切片：%q", got)
	}
}

func TestOutOfRangeKindIsIgnored(t *testing.T) {
	st := New(Options{Mode: ModeAll})
	sn := st.Begin("r1")
	sn.Add(Kind(-1), []byte("x"))
	sn.Add(kindCount, []byte("x"))
	sn.Set(Kind(99), []byte("x"))
	sn.Finish(true)
	snap, _ := st.Get("r1")
	for i := range snap.Bodies {
		if len(snap.Bodies[i].Bytes) != 0 {
			t.Errorf("越界 kind 写进了 body %d", i)
		}
	}
}
