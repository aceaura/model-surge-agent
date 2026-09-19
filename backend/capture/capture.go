// Package capture 保留一次请求在转换链上的四个 wire 形态，供事后排查
// 「转换到底破坏了什么」。
//
// 流水记的是**结论**：sanitized 说修了什么、lossy 说丢了什么、error_code
// 说哪类错。这些都是转换层自己判断出来的。当那个判断本身错了的时候，
// 流水里没有任何东西能看——只有原始字节能说明是哪一步坏的。
//
// 四体都是 wire 字节，不含中立表示：后者是 Go 结构体，序列化它需要一套
// 只为调试存在的编解码，而它的内容可由前后两体推出。
//
// **捕获只含 body，永不含请求头。** 上游凭据来自调度层、只写进 http.Request，
// 让它进捕获等于把一个排查设施变成凭据泄露面。
package capture

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// Mode 是捕获开关的三态。
type Mode string

const (
	// ModeOff 完全关闭：不分配缓冲、不产生开销。
	ModeOff Mode = "off"
	// ModeErrors 只留失败的请求。缓冲在内存里，成功即丢弃。
	ModeErrors Mode = "errors"
	// ModeAll 成功与失败都留。
	ModeAll Mode = "all"
)

// ParseMode 解析开关值。
//
// 非法值报错而不是回落到 off：静默回落的后果是运维以为捕获开着，
// 等出了故障才发现什么都没留——那时故障已经过去了。
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case "":
		return ModeOff, nil
	case ModeOff, ModeErrors, ModeAll:
		return Mode(s), nil
	default:
		return ModeOff, fmt.Errorf("capture mode %q is not one of off, errors, all", s)
	}
}

// Kind 标识四体中的哪一个。
type Kind int

const (
	// ClientRequest 是客户端发来的请求体（解压、去 BOM 之后）。
	ClientRequest Kind = iota
	// UpstreamRequest 是发给上游的 wire body（参数覆盖之后、真正写进请求的那份）。
	UpstreamRequest
	// UpstreamResponse 是上游回的原始字节（切帧之前，未经任何解码）。
	UpstreamResponse
	// ClientResponse 是回给客户端的字节（出站编码之后，与客户端实收一致）。
	ClientResponse
	kindCount
)

// 默认上限。
const (
	// DefaultMaxBody 是每一体的字节上限。
	//
	// 1 MB：足够装下一份塞满上下文的请求（那类请求的诊断价值在头部的模型名、
	// 参数与工具声明上），又不至于让 32 条捕获吃掉 128 MB。
	DefaultMaxBody = 1 << 20
	// DefaultMaxEntries 是保留的捕获条数上限。
	DefaultMaxEntries = 32
)

// Options 是捕获的可调参数。零值取默认。
type Options struct {
	Mode       Mode
	MaxBody    int
	MaxEntries int
}

// body 是四体中的一体。
type body struct {
	buf []byte
	// truncated 为真表示超过上限后被截断。
	//
	// 保留**前段**而不是后段：请求体的诊断价值在头部（模型名、参数、工具声明），
	// 响应流的头部则是 message_start 与首个内容块，而转换 bug 绝大多数在流的
	// 开头就已显形。只留尾部会把 message_start 挤掉，而那一帧常常正是问题所在。
	truncated bool
	// dropped 是被截断掉的字节数，让排查的人知道自己少看了多少。
	dropped int
}

// Session 是一次请求独占的捕获缓冲。
//
// nil 接收者上的每个方法都安全返回：off 档 Begin 返回 nil，于是调用点不需要
// 任何 if 分支，也就不会有人漏写那个 if。这与本仓 *cache.Cache 的既有写法一致。
type Session struct {
	store     *Store
	requestID string
	at        time.Time
	maxBody   int

	mu     sync.Mutex
	bodies [kindCount]body
}

// Store 持有已保留的捕获。进程级，由 main 装配一个。
type Store struct {
	opts Options

	mu sync.Mutex
	// order 是 requestID 的插入序，用于淘汰最旧。
	order []string
	kept  map[string]*Snapshot
}

// New 建捕获存储。Mode 为 off 时返回一个仍可安全调用的 Store：
// 它的 Begin 恒返回 nil，读取端点恒返回空列表。
//
// 不在 off 档返回 nil *Store：管理面持有它并会调 List，
// 让两处各自判 nil 不如让这一处的方法本身就是空操作。
func New(opts Options) *Store {
	if opts.MaxBody <= 0 {
		opts.MaxBody = DefaultMaxBody
	}
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = DefaultMaxEntries
	}
	if opts.Mode == "" {
		opts.Mode = ModeOff
	}
	return &Store{opts: opts, kept: map[string]*Snapshot{}}
}

// Mode 汇报当前开关，供管理面在列表为空时区分「关着」与「没有失败请求」。
func (s *Store) Mode() Mode {
	if s == nil {
		return ModeOff
	}
	return s.opts.Mode
}

// Begin 为一次请求开一个捕获会话。off 档返回 nil。
func (s *Store) Begin(requestID string) *Session {
	if s == nil || s.opts.Mode == ModeOff {
		return nil
	}
	return &Session{
		store:     s,
		requestID: requestID,
		at:        time.Now(),
		maxBody:   s.opts.MaxBody,
	}
}

// Add 往某一体追加字节。
//
// 复制而不是持有调用方的切片：出站 wire body 与上游读缓冲都会被复用，
// 持有引用会让捕获里的内容在之后被悄悄改写。
func (sn *Session) Add(kind Kind, p []byte) {
	if sn == nil || kind < 0 || kind >= kindCount || len(p) == 0 {
		return
	}
	sn.mu.Lock()
	defer sn.mu.Unlock()
	b := &sn.bodies[kind]
	room := sn.maxBody - len(b.buf)
	if room <= 0 {
		b.truncated = true
		b.dropped += len(p)
		return
	}
	if len(p) > room {
		b.buf = append(b.buf, p[:room]...)
		b.truncated = true
		b.dropped += len(p) - room
		return
	}
	b.buf = append(b.buf, p...)
}

// Set 覆盖某一体。
//
// 出站 wire body 用它而不用 Add：换目标重试时诊断的对象是**最终发出去的
// 那一次**，累加会把一份没被采用的 body 拼在前面，读的人分不开两份。
// 上游字节与回客户端字节反过来必须累加——那是同一个流的连续片段。
func (sn *Session) Set(kind Kind, p []byte) {
	if sn == nil || kind < 0 || kind >= kindCount {
		return
	}
	sn.mu.Lock()
	defer sn.mu.Unlock()
	b := &sn.bodies[kind]
	*b = body{}
	if len(p) > sn.maxBody {
		b.buf = append(b.buf, p[:sn.maxBody]...)
		b.truncated = true
		b.dropped = len(p) - sn.maxBody
		return
	}
	b.buf = append(b.buf, p...)
}

// Writer 把某一体包成 io.Writer，供 io.TeeReader 之类的既有管道直接用。
//
// nil 会话回 io.Discard 而不是 nil：调用点通常是 io.TeeReader 的第二个参数，
// 传 nil 进去会在第一次读时 panic，而那一刻离装配点已经很远了。
func (sn *Session) Writer(kind Kind) io.Writer {
	if sn == nil {
		return io.Discard
	}
	return kindWriter{sn: sn, kind: kind}
}

type kindWriter struct {
	sn   *Session
	kind Kind
}

// Write 恒报写入成功。
//
// 捕获是旁路：把它的截断或上限当成写失败会让 TeeReader 中断真正的数据流，
// 一个排查设施反倒毁掉被排查的请求。
func (w kindWriter) Write(p []byte) (int, error) {
	w.sn.Add(w.kind, p)
	return len(p), nil
}

// Finish 按开关与是否失败决定保留还是丢弃。
//
// failed 由调用方给，而不是在这里判 outcome：判定要用的是 rec.ErrorCode
// 是否非空（它恰好在且仅在错误路径上被设），而 outcome 的 retrying 是中间态、
// committed 后的失败还带着 usage——把那套语义搬进来会让两处各自漂移。
func (sn *Session) Finish(failed bool) {
	if sn == nil {
		return
	}
	if sn.store.opts.Mode == ModeErrors && !failed {
		// 丢弃：不留任何引用，缓冲可被回收。
		sn.mu.Lock()
		sn.bodies = [kindCount]body{}
		sn.mu.Unlock()
		return
	}
	sn.store.keep(sn.snapshot())
}

// Body 是快照里的一体。
type Body struct {
	Bytes     []byte
	Truncated bool
	Dropped   int
}

// Snapshot 是一次请求的四体快照，供管理面读取。
type Snapshot struct {
	RequestID string
	At        time.Time
	Bodies    [kindCount]Body
}

func (sn *Session) snapshot() *Snapshot {
	sn.mu.Lock()
	defer sn.mu.Unlock()
	out := &Snapshot{RequestID: sn.requestID, At: sn.at}
	for i := range sn.bodies {
		b := sn.bodies[i]
		out.Bodies[i] = Body{Bytes: b.buf, Truncated: b.truncated, Dropped: b.dropped}
	}
	return out
}

func (s *Store) keep(snap *Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.kept[snap.RequestID]; !exists {
		s.order = append(s.order, snap.RequestID)
	}
	s.kept[snap.RequestID] = snap
	// 每次写入最多新增一条，所以一次淘汰就够。
	if len(s.order) > s.opts.MaxEntries {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.kept, oldest)
	}
}

// List 返回已保留的捕获，最新在前。
func (s *Store) List() []*Snapshot {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Snapshot, 0, len(s.order))
	for i := len(s.order) - 1; i >= 0; i-- {
		if snap, ok := s.kept[s.order[i]]; ok {
			out = append(out, snap)
		}
	}
	return out
}

// Get 取一条捕获。第二个返回值为假表示没有。
func (s *Store) Get(requestID string) (*Snapshot, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, ok := s.kept[requestID]
	return snap, ok
}
