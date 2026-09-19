package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
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
	notes  []string
	body   io.Closer
	cancel context.CancelFunc
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

	// 单独的 cancel：流读完或出错时要能立刻掐断连接，
	// 不然 committed 后失败的连接会挂到客户端上下文结束。
	streamCtx, cancel := context.WithCancel(ctx)

	req, err := http.NewRequestWithContext(streamCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, ir.NewError(ir.ErrInternal, 0, "", fmt.Sprintf("build upstream request: %v", err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	// 客户端声明排在凭据头之前：调度层的头来自运维配置，
	// 运维意图优先于客户端声明。
	if de, ok := outbound.(codec.DeclarationEncoder); ok {
		for k, v := range de.DeclarationHeaders(decls) {
			req.Header.Set(k, v)
		}
	}
	// 凭据头来自调度层，只写进这个 http.Request，不入库不落日志。
	for k, v := range target.Headers {
		req.Header.Set(k, v)
	}

	upstreamStart := p.now()
	resp, err := p.client().Do(req)
	// 累加在判错之前，且终点是响应头到达而不是首帧解码出来：后者已经有
	// FirstTokenMS，且首帧里含上游的思考时间——那是生成成本不是连接成本。
	// 响应头这一刻正是 ResponseHeaderTimeout 约束的那一刻，两者对齐，
	// 运维看到 upstream_ms 逼近那个阈值就知道该调哪个参数。
	rec.UpstreamMS += msSince(upstreamStart, p.now())
	if err != nil {
		cancel()
		// 连接层与「上游明确地不行」分开归因：前者上游可能完全健康，
		// 记成它的失败会让一条死连接把健康账号推向冷却。
		if isTransportError(err) {
			return nil, ir.NewError(ir.ErrTransport, 0, "",
				fmt.Sprintf("upstream connection failed: %v", err))
		}
		return nil, ir.NewError(ir.ErrUpstream, 0, "", fmt.Sprintf("upstream unreachable: %v", err))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		// 错误体也是上游回的字节，而且正是最需要看的一类：DecodeError
		// 会把它归一化成一个 ir.Error，归错的时候只有原文能说明它到底说了什么。
		capt.Add(capture.UpstreamResponse, raw)
		cancel()
		return nil, outbound.DecodeError(resp.StatusCode, resp.Header, raw)
	}

	// 上游是否真的在发流，与客户端要不要流无关：兼容层网关忽略
	// stream:true 回一整份 JSON 是常见形态，按 SSE 去切它会一帧都读不出。
	if !isEventStream(resp.Header.Get("Content-Type")) {
		return adoptWholeResponse(resp, outbound, cancel, capt)
	}

	return &upstream{
		// 旁挂在 resp.Body 外面而不是改 FrameScanner：切帧属于 codec，
		// 让它知道捕获会把一个纯函数层绑上排查设施的生命周期。
		scanner: codec.NewFrameScanner(io.TeeReader(resp.Body, capt.Writer(capture.UpstreamResponse))),
		decoder: outbound.NewStreamDecoder(),
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

// maxWholeResponseBytes 是整份响应的读取上限。
// 与 SSE 单帧上限同量级：一份完整响应的合理上界不该比单帧宽松。
const maxWholeResponseBytes = 32 << 20

// adoptWholeResponse 把上游的整份响应读进来，投影成事件序列。
func adoptWholeResponse(resp *http.Response, outbound codec.OutboundCodec,
	cancel context.CancelFunc, capt *capture.Session) (*upstream, *ir.Error) {

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxWholeResponseBytes+1))
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
		return &upstream{cancel: cancel}, nil
	}
	decoded, decodeNotes, err := decodeWholeResponse(outbound, raw)
	if err != nil {
		cancel()
		return nil, ir.NewError(ir.ErrUpstream, 0, "",
			fmt.Sprintf("decode upstream response: %v", err))
	}
	return &upstream{
		replay: ir.ResponseEvents(decoded),
		notes:  append([]string{codec.UpstreamIgnoredStreamNote}, decodeNotes...),
		cancel: cancel,
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
