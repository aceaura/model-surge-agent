package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aceaura/model-surge-agent/backend/capture"
	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
)

// upstream 是一条已建立的上游流。
type upstream struct {
	scanner *codec.FrameScanner
	decoder codec.StreamDecoder
	// replay 非空表示上游回的不是 SSE，而是一份完整响应，
	// 已投影成事件序列，read 直接回放它。
	replay []ir.Event
	// notes 是建流阶段产生的有损说明，由调用方并进流水。
	notes []string
	// forwardHeaders 是允许回传给客户端的上游响应头，白名单筛过。
	forwardHeaders http.Header
	body           io.Closer
	cancel         context.CancelFunc
}

func (u *upstream) Close() {
	if u.body != nil {
		_ = u.body.Close()
	}
	if u.cancel != nil {
		u.cancel()
	}
}

// open 发出上游请求并确认拿到了 2xx 与流式响应体。
// 此时还没读任何帧：解码首帧的成败才决定要不要换目标。
// rec 只用于累加 UpstreamMS。计时贴在 client.Do 两侧而不是由 attempt 在
// 外面包住整个 open：包在外面的话，将来有人在 open 里加一段本地预处理，
// 那段耗时会被静默算成上游耗时，而这种漂移在读代码时看不出来。
func (p *Pipeline) open(ctx context.Context, outbound codec.OutboundCodec,
	target relayclient.Target, body []byte, decls codec.Declarations,
	rec *Record, capt *capture.Session) (*upstream, *ir.Error) {

	url, extra := outbound.Endpoint(target.BaseURL, target.NativeModel, true)
	// 空串是出站 codec 说「这个模型名拼不出安全的 URL」。归 upstream 而不是
	// internal：这是目标配置的问题，而换目标会换模型名，下一个可能是好的。
	// 不带模型名进错误文案——它由调度层给，流水里的 model_id 已经记了它。
	if url == "" {
		return nil, ir.NewError(ir.ErrUpstream, 0, "",
			"target model name cannot be placed in the upstream URL")
	}

	// 单独的 cancel：流读完或出错时要能立刻掐断连接，
	// 不然 committed 后失败的连接会挂到客户端上下文结束。
	streamCtx, cancel := context.WithCancel(ctx)
	// 重定向策略的说明收集口挂在 ctx 上：客户端是进程级共享的，
	// 策略函数不能持有本次请求的状态。
	streamCtx, sink := withRedirectSink(streamCtx)

	// 请求是否已完整交给上游。失败时它决定要不要换目标重发：已经发出去的
	// 请求，上游可能已经生成完并计了费，重发就是第二份账单。
	//
	// 用 httptrace 而不是推断：标准库内部有连接复用、h2 多路复用与请求重放，
	// 从错误文本反推「发出去了吗」每一条都是猜。探针实测这个回调在
	// ResponseHeaderTimeout 与「上游读完请求就断」两种失败上都已触发，
	// 而拨号被拒与 DNS 失败上没有。
	var wroteRequest atomic.Bool
	streamCtx = httptrace.WithClientTrace(streamCtx, &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) { wroteRequest.Store(true) },
	})

	req, err := http.NewRequestWithContext(streamCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, ir.NewError(ir.ErrInternal, 0, "", fmt.Sprintf("build upstream request: %v", err))
	}
	// 这两行是代码而不是配置，直接写：改它们要过评审，不必受运行时保护。
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	// 下面三层的头都来自配置（端点定义、客户端声明、调度层下发），
	// 一律过保护集合：其中任一写进 Accept-Encoding 都会静默关掉透明解压。
	var notes []string
	for k, v := range extra {
		notes = setOutboundHeader(req, k, v, notes)
	}
	// 客户端声明排在凭据头之前：调度层的头来自运维配置，
	// 运维意图优先于客户端声明。
	if de, ok := outbound.(codec.DeclarationEncoder); ok {
		for k, v := range de.DeclarationHeaders(decls) {
			notes = setOutboundHeader(req, k, v, notes)
		}
	}
	// 凭据头来自调度层，只写进这个 http.Request，不入库不落日志。
	for k, v := range target.Headers {
		notes = setOutboundHeader(req, k, v, notes)
	}

	upstreamStart := p.now()
	resp, err := p.client().Do(req)
	// 累加在判错之前，且终点是响应头到达而不是首帧解码出来：后者已经有
	// FirstTokenMS，且首帧里含上游的思考时间——那是生成成本不是连接成本。
	// 响应头这一刻正是 ResponseHeaderTimeout 约束的那一刻，两者对齐，
	// 运维看到 upstream_ms 逼近那个阈值就知道该调哪个参数。
	upstreamMS := msSince(upstreamStart, p.now())
	rec.UpstreamMS += upstreamMS
	// 同一次尝试内 open 只调一次，所以本次值直接赋而不是累加。
	rec.attemptUpstreamMS = upstreamMS
	// 重定向策略的说明直接并进 rec.Lossy，不搭 notes 那趟车。
	//
	// notes 只在 open 成功返回时才被调用方取走，而重定向的说明几乎总是产生在
	// 失败那一侧——摘掉凭据之后这一跳就被拒了。搭 notes 的话，唯一会产生这条
	// 说明的场景恰好是它一定丢掉的场景。
	for _, note := range sink.drain() {
		rec.Lossy = codec.MergeNotes(rec.Lossy, []string{note})
	}
	if err != nil {
		cancel()
		// 重定向类失败必须排在 isTransportError 之前：我们的哨兵被 *url.Error
		// 裹着，而它不是 net.Error，当前分支会把它归到「upstream unreachable」——
		// kind 恰好对了但文本是错的。
		//
		// 文本一个字都不从原始错误里取：Do 返回的错误内嵌重定向目标 URL 含
		// query（探针实测 `Post "/next?key=sk-inquery": ...`）。
		if reason, ok := redirectFailure(err); ok {
			// ErrUpstream 本身就判可重试，不再包 retryableErr：
			// 那是一个空操作，而空操作会让读的人以为这里有一个
			// 与 kind 无关的额外判断。
			//
			// 刻意不标 SideEffectRisk：请求确实已经发出去了，但上游回的是
			// 一个重定向而不是一次生成——它没处理这个请求，也就没有计费。
			// 与 3xx 走状态码那条路同一个道理。标上的话一个配错了 Location
			// 的目标会让整个请求直接失败而不是换个目标，反而更糟。
			return nil, ir.NewError(ir.ErrUpstream, 0, "", reason)
		}
		// 连接层与「上游明确地不行」分开归因：前者上游可能完全健康，
		// 记成它的失败会让一条死连接把健康账号推向冷却。
		//
		// 两条路径都走净化：原始 error 是 *url.Error，它内嵌完整请求 URL 含
		// query，而这个 message 会流到客户端可见的错误体里。
		reason := sanitizeTransportError(err)
		risk := wroteRequest.Load()
		// 不可达必须排在 isTransportError 之前：NXDOMAIN 是 *net.OpError 包
		// *net.DNSError，下面那个分支会把它认下并归成「换条连接就好」，
		// 于是一个域名写错的目标永远不计失败、永不冷却。
		if isUnreachableTarget(err) {
			return nil, withSideEffectRisk(ir.NewError(ir.ErrUpstream, 0, "",
				"upstream target unreachable: "+reason), risk)
		}
		if isTransportError(err) {
			return nil, withSideEffectRisk(ir.NewError(ir.ErrTransport, 0, "",
				"upstream connection failed: "+reason), risk)
		}
		return nil, withSideEffectRisk(ir.NewError(ir.ErrUpstream, 0, "",
			"upstream unreachable: "+reason), risk)
	}
	// 3xx 排在状态码闸门之前：闸门那一段要读体、解压、DecodeError，
	// 而一个没有可用 Location 的 3xx 的体不值得走这一套。
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		drainAndClose(resp.Body)
		cancel()
		return nil, ir.NewError(ir.ErrUpstream,
			resp.StatusCode, "", unfollowableRedirect)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 错误体同样要解压：压缩的错误体交给 DecodeError 只会解不出结构、
		// 归成一个笼统的上游错误，而限流与配额耗尽正是靠这个体区分的。
		// 解压失败时退回原始体：归因已经是「上游不行」，再因为解压失败
		// 换一条错误路径只会丢掉状态码这条更硬的信息。
		var errBody io.Reader = resp.Body
		if dec, decErr := decodeBody(resp.Header, resp.Body); decErr == nil {
			errBody = dec
		}
		raw, _ := io.ReadAll(io.LimitReader(errBody, 64<<10))
		drainAndClose(resp.Body)
		// 错误体也是上游回的字节，而且正是最需要看的一类：DecodeError
		// 会把它归一化成一个 ir.Error，归错的时候只有原文能说明它到底说了什么。
		capt.Add(capture.UpstreamResponse, raw)
		cancel()
		return nil, outbound.DecodeError(resp.StatusCode, resp.Header, raw)
	}

	// 解压挂在捕获的里侧：捕获要留的是能读的字节。存压缩字节等于把
	// 「上游到底回了什么」这条最后的线索变成一段谁也看不懂的二进制，
	// 而排查这类问题时恰好只有它可看。
	decoded, encErr := decodeBody(resp.Header, resp.Body)
	if encErr != nil {
		_ = resp.Body.Close()
		cancel()
		return nil, encErr
	}

	if !isEventStream(resp.Header.Get("Content-Type")) {
		return adoptWholeResponse(resp, decoded, outbound, cancel, capt, notes)
	}

	return &upstream{
		forwardHeaders: forwardableHeaders(resp.Header),
		// 旁挂在 resp.Body 外面而不是改 FrameScanner：切帧属于 codec，
		// 让它知道捕获会把一个纯函数层绑上排查设施的生命周期。
		scanner: codec.NewFrameScanner(io.TeeReader(decoded, capt.Writer(capture.UpstreamResponse))),
		decoder: outbound.NewStreamDecoder(),
		notes:   notes,
		body:    resp.Body,
		cancel:  cancel,
	}, nil
}

// isEventStream 判断上游是否真的在发 SSE。
//
// 只看 media type，忽略 charset 等参数。头缺失或解不动时按 SSE 处理：
// 绝大多数缺头的上游发的是正常 SSE，而错判成整份响应会把真流缓冲成
// 一整份、毁掉逐字输出。
func isEventStream(ct string) bool {
	if ct == "" {
		return true
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return true
	}
	return strings.EqualFold(mt, "text/event-stream")
}

// forwardedHeaderPrefixes 是允许从上游回传给客户端的响应头前缀。
//
// 白名单而非黑名单。黑名单每漏一项都是一次泄露——Set-Cookie、上游的内部
// 追踪头、以及上游将来新加的任何头；白名单漏一项只是少一个提示。
// 参考仓库 sub2api 的 gemini handler 走的是黑名单（只跳三个 hop-by-hop
// 头、其余全传），不跟。
//
// 限流头值得传：Claude Code 一类客户端靠 unified-remaining 自适应节流，
// 拿不到就只能全速打到 429 才退避，表现为周期性硬撞限流。
//
// 前缀而不是全名列举：anthropic 的限流头是一族（requests-remaining、
// tokens-remaining、unified-remaining、unified-reset……）且会增加，
// 列举等于上游每加一个我们就要跟改一次。
var forwardedHeaderPrefixes = []string{
	"anthropic-ratelimit-",
	"x-ratelimit-",
}

// forwardableHeaders 从上游响应头里筛出可以回传的那些。
//
// Retry-After 刻意不在白名单里：它已经由 codec/ratelimit 解析、按我们自己
// 的口径写出，两条路都写会产生两个值。
func forwardableHeaders(h http.Header) http.Header {
	var out http.Header
	for k, vs := range h {
		lower := strings.ToLower(k)
		for _, prefix := range forwardedHeaderPrefixes {
			if !strings.HasPrefix(lower, prefix) {
				continue
			}
			if out == nil {
				out = make(http.Header, 4)
			}
			out[http.CanonicalHeaderKey(k)] = append([]string(nil), vs...)
			break
		}
	}
	return out
}

// applyForwardedHeaders 把筛过的上游头写进客户端响应。
//
// 必须在 WriteHeader 之前调用：之后写的头不会发出去，而且不报错——
// 那种失效只能靠逐个字段比对响应头才看得出来。
func applyForwardedHeaders(w http.ResponseWriter, up *upstream) {
	if up == nil {
		return
	}
	for k, vs := range up.forwardHeaders {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
}

// maxDrainBytes 是关闭响应体之前排空的上限。
//
// 必须有界：无界排空等于让上游用一个超长响应体把我们挂在这里。超过上限就
// 直接关掉、接受丢这一条连接——那已经是异常上游，不值得为它继续读。
const maxDrainBytes = 1 << 20

// drainAndClose 把响应体剩余部分排空之后关闭。
//
// 不排空的话标准库不会把这条连接放回空闲池。本机探针：200KB 的错误体、
// 连续五次同样的 429，只读前 64KB 就关的话建了五条新连接，排空后只建一条。
// 于是上游进 429 风暴（错误体常带长 HTML）时每次失败丢一条连接，
// MaxIdleConnsPerHost 形同未配、TLS 握手随失败率线性上涨，而池饱和指标
// 看着正常——池里没连接不是因为满，是因为没人放回来，于是调参毫无效果。
//
// 读 resp.Body 而不是解压后的 reader：要放回池里的是底层字节流，
// 读解压流在压缩比高时读不完底层那些字节。
func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxDrainBytes))
	_ = body.Close()
}

// looksLikeHTML 判断响应体是不是一张网页。
//
// 只看开头的记号，不做完整解析：目的是给错误消息一个正确的归因，
// 而不是理解这个页面。大小写都认——WAF 与门户的拦截页两种写法都有。
func looksLikeHTML(raw []byte) bool {
	head := bytes.ToLower(bytes.TrimSpace(raw))
	if len(head) > 512 {
		head = head[:512]
	}
	return bytes.HasPrefix(head, []byte("<!doctype html")) ||
		bytes.HasPrefix(head, []byte("<html"))
}

// maxWholeResponseBytes 是整份响应的读取上限。
// 与 SSE 单帧上限同量级：一份完整响应的合理上界不该比单帧宽松。
const maxWholeResponseBytes = 32 << 20

// adoptWholeResponse 把上游的整份响应读进来，投影成事件序列。
// body 是已经解过压的响应体；关闭仍用 resp.Body。
// notes 是建流阶段已积累的说明（如被丢弃的出站头），这条分支要把自己的
// 说明接在它后面而不是覆盖它：两类问题可以同时存在，丢一类就少一条线索。
func adoptWholeResponse(resp *http.Response, body io.Reader, outbound codec.OutboundCodec,
	cancel context.CancelFunc, capt *capture.Session, notes []string) (*upstream, *ir.Error) {

	raw, err := io.ReadAll(io.LimitReader(body, maxWholeResponseBytes+1))
	// 这条路上不排空：上面已经读到底了（超出上限那一种排 1MB 也到不了 EOF，
	// 照样复用不了），排空在这里恒为空操作。
	_ = resp.Body.Close()
	// 这条分支也要捕获：上游忽略 stream:true 回整份 JSON 是常见形态，
	// 漏掉它会让「非 SSE 上游」这一类问题恰好没有原始字节可看。
	capt.Add(capture.UpstreamResponse, raw)
	if err != nil {
		cancel()
		return nil, ir.NewError(ir.ErrUpstream, 0, "",
			fmt.Sprintf("read upstream response: %v", err))
	}
	if len(raw) > maxWholeResponseBytes {
		cancel()
		return nil, ir.NewError(ir.ErrUpstream, 0, "", "upstream response exceeds the size limit")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		// 空体与「一帧都没发」同口径：都是上游在写内容之前就结束了，
		// 可以换个目标重来。留 replay 为 nil 让 read 走那条既有路径。
		return &upstream{notes: notes,
			forwardHeaders: forwardableHeaders(resp.Header), cancel: cancel}, nil
	}
	decoded, decodeNotes, err := decodeWholeResponse(outbound, raw)
	if err != nil {
		cancel()
		// HTML 单独归因。不点明的话运维在流水里看到的是
		// `invalid character '<' looking for beginning of value`，
		// 那句话指向「我们的解码器坏了」，而真实原因是有台中间设备在拦。
		// kind 仍是可重试的 ErrUpstream：换个目标确实可能绕过那台设备。
		if looksLikeHTML(raw) {
			return nil, ir.NewError(ir.ErrUpstream, 0, "",
				"upstream returned an HTML page instead of a response; "+
					"a proxy or WAF is likely intercepting")
		}
		// 带上 Content-Type：一个头就能把「解码器坏了」与「上游回了别的
		// 东西」分开，而这两者现在的文本一模一样。
		return nil, ir.NewError(ir.ErrUpstream, 0, "",
			fmt.Sprintf("decode upstream response (content-type %q): %v",
				resp.Header.Get("Content-Type"), err))
	}
	return &upstream{
		replay:         ir.ResponseEvents(decoded),
		forwardHeaders: forwardableHeaders(resp.Header),
		notes:          append(append(notes, codec.UpstreamIgnoredStreamNote), decodeNotes...),
		cancel:         cancel,
	}, nil
}

// decodeWholeResponse 优先走解码器的有损出口。
//
// 不实现该出口的协议等价于「解码不丢任何东西」，回落到普通解码。
func decodeWholeResponse(outbound codec.OutboundCodec, raw []byte) (*ir.Response, []string, error) {
	if d, ok := outbound.(codec.LossyResponseDecoder); ok {
		return d.DecodeResponseLossy(raw)
	}
	resp, err := outbound.DecodeResponse(raw)
	return resp, nil, err
}

// fallbackClient 是没装配 HTTP 时用的客户端。
//
// 回落到本层的默认配置而不是 http.DefaultClient：后者 PerHost 只留 2 条
// 空闲连接、且响应头等待不设限，装配漏一行就把整层配置静默降级成旧行为，
// 而那是编译器与所有单测都看不见的。
var fallbackClient = NewHTTPClient(TransportOptions{})

func (p *Pipeline) client() *http.Client {
	if p.HTTP != nil {
		return p.HTTP
	}
	return fallbackClient
}

// frame 是从上游读到的一帧解码结果。
type frame struct {
	events []ir.Event
	err    *ir.Error
	// done 表示上游流已结束。
	done bool
}

// read 在后台读流并把解码结果送进 channel。
//
// 单独一个 goroutine 是为了给首字节与空闲设超时：net/http 的 Body.Read
// 在服务端不发数据时会一直阻塞，只有 select 能加上时限。
func (u *upstream) read(ctx context.Context) <-chan frame {
	out := make(chan frame, 8)
	go func() {
		defer close(out)
		if u.replay != nil {
			// 上游回的是整份响应。一次送出全部事件而不逐条：这不是真流，
			// 假装逐字到达只会让客户端以为有过增量。投影里已带终止事件，
			// 不能再调 decoder.Finish() 补一个。
			send(ctx, out, frame{events: u.replay, done: true})
			return
		}
		if u.scanner == nil {
			// 上游 200 但整份响应体是空的：与「一帧都没发」同口径。
			send(ctx, out, frame{done: true})
			return
		}
		sawFrame := false
		for u.scanner.Scan() {
			sawFrame = true
			f := u.scanner.Frame()
			events, err := u.decoder.Feed(f.Event, f.Data)
			if err != nil {
				send(ctx, out, frame{err: asIRError(err, ir.ErrUpstream)})
				return
			}
			if len(events) > 0 && !send(ctx, out, frame{events: events}) {
				return
			}
		}
		if err := u.scanner.Err(); err != nil {
			send(ctx, out, frame{err: ir.NewError(ir.ErrUpstream, 0, "",
				fmt.Sprintf("upstream stream broke: %v", err))})
			return
		}
		if !sawFrame {
			// HTTP 200 但 body 一帧都没有：上游在写内容之前就断了。
			// 此时不能让解码器补终止事件——那会让上层以为目标已经响应而
			// 锁定它，实际这是截断，还能换个目标重来。
			send(ctx, out, frame{done: true})
			return
		}
		// 上游没发终止事件时解码器补一个，下游才知道流结束了。
		send(ctx, out, frame{events: u.decoder.Finish(), done: true})
	}()
	return out
}

func send(ctx context.Context, out chan<- frame, f frame) bool {
	select {
	case out <- f:
		return true
	case <-ctx.Done():
		return false
	}
}

func (p *Pipeline) firstTokenTimeout() time.Duration {
	if p.Opts.FirstTokenTimeout > 0 {
		return p.Opts.FirstTokenTimeout
	}
	return 60 * time.Second
}

func (p *Pipeline) idleTimeout() time.Duration {
	if p.Opts.IdleTimeout > 0 {
		return p.Opts.IdleTimeout
	}
	return 120 * time.Second
}

// defaultHeartbeatInterval 是保活帧间隔。
//
// 15s 而非参考实现的 10s，但必须显著小于中间设施最常见的 30s 空闲阈值，
// 且必须小于首帧超时（默认 60s）——否则长思考期内一个心跳都发不出，
// 配了等于没配。
const defaultHeartbeatInterval = 15 * time.Second

// heartbeatInterval 零值取默认、负值表示显式关闭，与连接层的
// durationOrDefault 同口径。
func (p *Pipeline) heartbeatInterval() time.Duration {
	return durationOrDefault(p.Opts.HeartbeatInterval, defaultHeartbeatInterval)
}
