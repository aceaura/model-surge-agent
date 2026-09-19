package pipeline

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/aceaura/model-surge-agent/backend/capture"
	"github.com/aceaura/model-surge-agent/backend/codec"
	"github.com/aceaura/model-surge-agent/backend/ir"
	"github.com/aceaura/model-surge-agent/backend/relayclient"
)

// incompleteStream 是流没说完就断掉时记入流水的错误码。
// 覆盖残缺工具入参与缺签名的未闭合推理块：两者都不能补个闭合帧当成功。
const incompleteStream = "incomplete_stream"

// bridge 把上游流转成客户端响应。
//
// 首帧成功解码之前不动 w：只有确认这个目标真的在出内容，才锁定它。
// 之前的失败都还能换目标重试，之后就只能把错误写进已开启的响应里。
func (p *Pipeline) bridge(ctx context.Context, w http.ResponseWriter, call Call, up *upstream,
	start time.Time, rec *Record) (string, attemptResult) {

	// clientCtx 是客户端请求本身的 ctx，单独留一份。
	// 下面那个派生 ctx 带自己的 cancel（用于收尾时掐断读协程），
	// 拿它判「是谁断的」会把我们自己的收尾也算成客户端取消。
	clientCtx := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	frames := up.read(ctx)

	var (
		encoder   codec.StreamEncoder
		agg       ir.Aggregator
		committed bool
		streamErr *ir.Error
		// tail 暂存上游的终止事件，等 finish 判完整性后再决定要不要发。
		// 一发出去客户端就认为这轮正常结束了，而「工具入参截断」这类残缺
		// 只有收完全流才能判出来，此时补救已经来不及。
		tail []ir.Event
	)
	if call.Stream {
		encoder = call.Inbound.NewStreamEncoder()
	}

	timeout := p.firstTokenTimeout()
	// 超时改成绝对时刻而不是每轮新起一个 time.After：心跳会让循环多醒几次，
	// 每轮重起的计时器等于被心跳一次次推后，于是一条彻底静默的上游流永远
	// 等不到空闲超时——加心跳恰好会废掉空闲超时，这是必须一起改的。
	// 只有真帧到达才推进它。
	deadline := time.Now().Add(timeout)

	// 非流式请求不建这个计时器：encoder 为 nil 时循环里那条分支本来也会
	// 直接跳过，但每个非流式请求白起一个 ticker 是纯开销。
	// 判据用 encoder 而不是 call.Stream：循环里那条分支判的就是 encoder，
	// 两处用同一个依据才不会出现「开了计时器却永远发不出」的错配。
	var heartbeat <-chan time.Time
	if hb := p.heartbeatInterval(); hb > 0 && encoder != nil {
		ticker := time.NewTicker(hb)
		defer ticker.Stop()
		heartbeat = ticker.C
	}

	for {
		var f frame
		var ok bool
		select {
		case <-heartbeat:
			// committed 之前不发：响应头还没写出，此时往 w 写会把状态码钉死在
			// 200，而这个阶段的失败本该换目标重试或回一个正确的 HTTP 错误码。
			if !committed || encoder == nil {
				continue
			}
			if err := writeHeartbeat(w, encoder, call.Capture); err != nil {
				return p.clientGone(&agg, rec)
			}
			// 刻意不推进 deadline：心跳是我们自己发的，它不是上游还活着的证据。
			continue

		case f, ok = <-frames:
			// 真帧到达才算上游还活着。
			deadline = time.Now().Add(timeout)
			if !ok {
				// channel 关了却没收到 done：读协程被 ctx 掐断了。
				// 先分清是谁断的——客户端自己走了就不是这个目标的故障，
				// 记成 abnormal 会累计失败计数把健康账号推向冷却。
				if clientCtx.Err() != nil {
					return p.clientGone(&agg, rec)
				}
				return p.finish(w, call, up, encoder, &agg, committed, tail,
					ir.NewError(ir.ErrUpstream, 0, "", "upstream stream ended without a terminator"), rec)
			}
		case <-time.After(time.Until(deadline)):
			kind := "first token"
			if committed {
				kind = "idle"
			}
			return p.finish(w, call, up, encoder, &agg, committed, tail,
				ir.NewError(ir.ErrTimeout, 0, "", kind+" timeout"), rec)
		}

		if f.err != nil {
			// 客户端取消会先让上游连接断开，于是 scanner 报一个读错误，
			// 而不是 channel 干净关闭。两条路径都要先分清是谁断的。
			if clientCtx.Err() != nil {
				return p.clientGone(&agg, rec)
			}
			return p.finish(w, call, up, encoder, &agg, committed, tail, f.err, rec)
		}

		for _, ev := range f.events {
			if ev.Type == ir.EvError {
				// 上游把错误放进了流内。已 committed 时同样只能流内表达。
				streamErr = ev.Err
				continue
			}
			agg.Add(ev)

			if !committed {
				// 首帧解码出了事件：从这里起目标锁定，HTTP 状态码不可再改。
				committed = true
				rec.Committed = true
				rec.FirstTokenMS = msSince(start, p.now())
				timeout = p.idleTimeout()
				if call.Stream {
					rec.StatusCode = http.StatusOK
					// 必须在 writeStreamHeaders 之前：它里面就 WriteHeader 了。
					applyForwardedHeaders(w, up)
					writeStreamHeaders(w)
				}
			}
			if ev.Type == ir.EvMessageDelta || ev.Type == ir.EvMessageStop {
				tail = append(tail, ev)
				continue
			}
			if encoder != nil {
				if err := writeEvents(w, encoder, ev, call.Capture, rec); err != nil {
					// 客户端自己断开，上游一直在正常出内容。记成 abnormal 会
					// 累计失败计数、把好目标推向冷却——客户端多按几次停止就能
					// 拖垮账号。按已收 usage 正常记账（上游照样计费）。
					//
					// error_code 要在这里写：committed 路径不经 fail，
					// 上层只填 outcome 与 usage，不写就是一条查不出原因的流水。
					return p.clientGone(&agg, rec)
				}
			}
		}

		if f.done {
			// 只在 done 上取解码器说明：读协程此时已送完最后一帧，不会再碰
			// 解码器。超时与断流路径上协程可能正在 Feed，那时读 notes 是数据竞争。
			if n, ok := up.decoder.(codec.StreamNotes); ok {
				rec.addResponseLossy(n.Notes()...)
			}
			return p.finish(w, call, up, encoder, &agg, committed, tail, streamErr, rec)
		}
	}
}

// finish 收尾：err 为 nil 是成功路径，否则按是否 committed 决定
// 错误落在流内还是当作可换目标的失败回给上层。
func (p *Pipeline) finish(w http.ResponseWriter, call Call, up *upstream,
	encoder codec.StreamEncoder, agg *ir.Aggregator, committed bool, tail []ir.Event,
	err *ir.Error, rec *Record) (string, attemptResult) {

	usage := usageOf(agg)
	if p.Opts.EstimateUsage && usage.OutputTokens == 0 {
		// 上游没给 usage：按字符数估一个，否则调度层的用量统计会永远是 0。
		resp := agg.Response()
		usage.OutputTokens = int64(ir.EstimateResponse(resp))
		rec.UsageEstimated = true
	}
	if p.Opts.EstimateUsage && usage.InputTokens == 0 {
		// 输入侧同样要兜：只兜 output 的话上游不报 usage 时整个输入维度恒为 0，
		// 调度层的用量累计会系统性漏掉它。
		//
		// 用调度方向（低估）而非公开方向：这个数字进的是配额累计，
		// 高估等于凭空吃掉用户的额度。
		//
		// 判 == 0 而不是 <= 0：负数是上游给了畸形数字，那是另一类问题，
		// 兜底会把它掩盖成一个看起来正常的值。
		usage.InputTokens = ir.EstimateRequest(call.Request)
		rec.UsageEstimated = true
	}

	if err == nil {
		if !committed {
			// HTTP 200 却一个事件都没解出来：上游在写 body 之前就断了，
			// 或返回了本协议解不动的东西。这是截断而非空回答，一个字节都
			// 还没写给客户端，换目标重来。
			return relayclient.OutcomeRetrying, attemptResult{
				err: ir.NewError(ir.ErrUpstream, 0, incompleteStream,
					"upstream produced no events"),
				usage: usage,
			}
		}
		// 断流留下了不能补闭合的块：残缺工具入参或缺签名的推理块。
		// 客户端会把它们存进历史，下一轮重放时整个请求都会被上游拒收。
		if unsafe := agg.UnsafeToClose(); len(unsafe) > 0 {
			msg := "incomplete stream: " + strings.Join(unsafe, ", ")
			if !call.Stream {
				// 非流式：一个字节都还没写出，可以换目标重来。
				// 这里不写 rec：重试成功后没有代码会把错误字段清掉。
				return relayclient.OutcomeRetrying, attemptResult{
					err:   ir.NewError(ir.ErrUpstream, 0, incompleteStream, msg),
					usage: usage,
				}
			}
			// 流式：内容已发出、状态码已定，收不回来了。补一个正常终止帧
			// 会把残缺内容伪装成完整回答，改为流内错误收尾——客户端据此
			// 知道这轮不可用，不会把毒历史存下来。
			err := ir.NewError(ir.ErrUpstream, 0, incompleteStream, msg)
			rec.ErrorCode = incompleteStream
			rec.ErrorMessage = msg
			writeStreamErrorEnd(w, encoder, err, call.Capture)
			return relayclient.OutcomeAbnormal, attemptResult{
				err: err, committed: true, usage: usage,
			}
		}
		// 未闭合的块全是文本：半句话是可接受的截断，补齐闭合帧照常终止，
		// 但终止原因要说成 max_tokens——报 end_turn 等于告诉客户端这段话
		// 说完了，它就不会去续写。
		if agg.HasOpenBlocks() && agg.Response().StopReason == "" {
			trunc := ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopMaxTokens}
			agg.Add(trunc)
			tail = append(tail, trunc)
		}
		p.writeSuccess(w, call, up, encoder, agg, tail, rec)
		return relayclient.OutcomeNormal, attemptResult{usage: usage}
	}

	if !committed {
		return outcomeFor(err), attemptResult{err: err, usage: usage}
	}

	// committed 之后 HTTP 状态已定：错误只能作为流内事件表达。
	// 编码器会在错误帧之前闭合未关的块，但不补正常终止帧——前者让客户端
	// 的块状态机收束，后者会让它把这轮当成功。
	rec.ErrorCode = string(err.Kind)
	rec.ErrorMessage = err.Message
	if encoder != nil {
		writeStreamErrorEnd(w, encoder, err, call.Capture)
	} else {
		// 非流式客户端：聚合到一半断了，没有半个响应可交，只能回错。
		// 状态码还没写出，所以这里仍能给出正确的 HTTP 错误。
		p.fail(w, call, rec, err)
	}
	return relayclient.OutcomeAbnormal, attemptResult{err: err, committed: true, usage: usage}
}

func (p *Pipeline) writeSuccess(w http.ResponseWriter, call Call, up *upstream,
	encoder codec.StreamEncoder, agg *ir.Aggregator, tail []ir.Event, rec *Record) {
	if encoder != nil {
		// tail 是暂存的上游终止事件，确认这轮完整之后才放行。
		for _, ev := range tail {
			if err := writeEvents(w, encoder, ev, call.Capture, rec); err != nil {
				return
			}
		}
		writeFrames(w, encoder.Finish(), call.Capture)
		flush(w)
		// Notes 在 Finish 之后取：收尾阶段本身也可能丢弃内容。
		if n, ok := encoder.(codec.StreamNotes); ok {
			rec.addResponseLossy(n.Notes()...)
		}
		return
	}
	// 非流式：把聚合结果编成一次性响应。对上游一律流式，
	// 所以这条路径复用同一套解码逻辑，不必单独实现非流式解码。
	body, notes, err := encodeResponseWithLossy(call.Inbound, agg.Response())
	if err != nil {
		p.fail(w, call, rec, asIRError(err, ir.ErrInternal))
		return
	}
	rec.addResponseLossy(notes...)
	rec.StatusCode = http.StatusOK
	applyForwardedHeaders(w, up)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	call.Capture.Add(capture.ClientResponse, body)
}

// clientGone 收尾客户端取消。
//
// 记 normal 而不是 abnormal：客户端自己走了不是这个目标的故障，累计失败
// 计数会让它无端冷却。usage 按已收事件结算——上游照样计费，不记等于漏账。
// 不换目标重试，也不往 w 写任何东西：客户端已经不要这个回答了。
//
// 与流式写失败那条分支（上面的 "client went away"）判的是同一件事，
// 区别只在观察点：那条要等写客户端失败才发现，而非流式请求在聚合完成前
// 一个字节都不写，只能从 ctx 观察到。
func (p *Pipeline) clientGone(agg *ir.Aggregator, rec *Record) (string, attemptResult) {
	err := ir.NewError(ir.ErrCanceled, 0, "", "client canceled the request")
	rec.ErrorCode = string(err.Kind)
	rec.ErrorMessage = err.Message
	return relayclient.OutcomeNormal, attemptResult{
		err: err, committed: true, usage: usageOf(agg),
	}
}

// writeStreamErrorEnd 用流内错误收尾。
//
// 走 encoder 而不是直接调 RenderStreamError：编码器要据此把自己标成
// 已错误终止，Finish 才不会再补一个正常终止帧，把残缺内容伪装成完整回答。
func writeStreamErrorEnd(w http.ResponseWriter, encoder codec.StreamEncoder, err *ir.Error,
	capt *capture.Session) {
	frames, encErr := encoder.Encode(ir.Event{Type: ir.EvError, Err: err})
	if encErr == nil {
		writeFrames(w, frames, capt)
	}
	writeFrames(w, encoder.Finish(), capt)
	flush(w)
}

func writeEvents(w http.ResponseWriter, encoder codec.StreamEncoder, ev ir.Event,
	capt *capture.Session, rec *Record) error {
	out, err := encoder.Encode(ev)
	if err != nil {
		// 编码失败不该中断整个流：跳过这个事件，其余内容照发。
		//
		// 但必须留说明：客户端收到的内容缺了一块，而流正常收束，不留说明的
		// 话这件事在诊断里完全不存在。带事件类型——跳掉一段文本与跳掉一次
		// 工具调用的后果差得远。
		//
		// 刻意不拼 err 的文本：mergedLossy 会去重，而错误措辞一变同一类
		// 跳过就会散成多条。
		rec.addResponseLossy("skipped response event " + string(ev.Type) +
			" (the client protocol could not encode it)")
		return nil
	}
	extendWriteDeadline(w)
	for _, f := range out {
		if _, err := w.Write(f); err != nil {
			return err
		}
		// 捕获在写成功之后：客户端真收到的才算回给客户端的字节。
		capt.Add(capture.ClientResponse, f)
	}
	flush(w)
	return nil
}

// writeHeartbeat 往已开启的流里写一个保活帧。
//
// 挡的是中间设施的空闲断连：反代、负载均衡与云网关普遍在 30~60s 无字节时
// 掐掉连接，而推理模型在长思考期间可以几分钟不出一个 token。被掐时客户端
// 看到的是连接异常中断，而上游其实一切正常、还在计费生成。
//
// 不进 agg、不进 tail、不动 usage：它不是内容，混进去会让记账多算、
// 让完整性判定看到一个不存在的事件。进 capture：客户端确实收到了这些字节，
// 排查「客户端说流里有奇怪帧」时需要它们在场。
//
// 写失败按客户端已走处理，与 writeEvents 同口径。
func writeHeartbeat(w http.ResponseWriter, encoder codec.StreamEncoder,
	capt *capture.Session) error {

	frame := codec.HeartbeatComment
	if h, ok := encoder.(codec.StreamHeartbeat); ok {
		// 编码器明确返回 nil 表示本协议不发保活，此时不能回落到注释帧——
		// 那会绕过它的判断。
		frame = h.HeartbeatFrame()
		if frame == nil {
			return nil
		}
	}
	extendWriteDeadline(w)
	if _, err := w.Write(frame); err != nil {
		return err
	}
	capt.Add(capture.ClientResponse, frame)
	flush(w)
	return nil
}

func writeFrames(w http.ResponseWriter, frames [][]byte, capt *capture.Session) {
	extendWriteDeadline(w)
	for _, f := range frames {
		if _, err := w.Write(f); err != nil {
			return
		}
		capt.Add(capture.ClientResponse, f)
	}
}

// streamWriteTimeout 限制单次写被慢客户端阻塞多久。
//
// 挡的是「连着但不读」：客户端把 TCP 接收窗口打满却不断开时，
// w.Write 会一直阻塞，而它是同步调用、不在任何 select 里——
// 空闲超时的计时器与客户端取消都观察不到，于是 goroutine、上游连接、
// 帧缓冲全部长期占用，上游那边还在计费生成。
//
// 30s 对正常客户端是三个数量级的余量（一次写是微秒级）。
const streamWriteTimeout = 30 * time.Second

// extendWriteDeadline 在每次写之前把写 deadline 往后推。
//
// 逐次推进而非一次性总时限：Server.WriteTimeout 从响应开始算，
// SSE 跑几分钟必然撞上。每帧推一次等价于「单次写不得阻塞超过 N 秒」，
// 读得正常的客户端永远不会触发。
//
// 不支持设 deadline 时静默跳过：httptest.ResponseRecorder 恒不支持，
// 而单元测试全走它，当成故障会让每帧都误判为写失败。
//
// 注意这条链路上每一层包装 ResponseWriter 的类型都必须提供 Unwrap，
// ResponseController 靠 Unwrap 方法链找真实连接，只做 struct embedding
// 会让它认不出底层、返回 ErrNotSupported——代码写了也不生效且不报错。
func extendWriteDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(streamWriteTimeout))
}

func writeStreamHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// 反代默认会缓冲 SSE，攒够一块才转发，客户端就看不到逐字输出了。
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flush(w)
}

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func usageOf(agg *ir.Aggregator) relayclient.Usage {
	u := agg.Response().Usage
	return relayclient.Usage{
		InputTokens:     u.InputTokens,
		OutputTokens:    u.OutputTokens,
		CacheReadTokens: u.CacheReadTokens,
	}
}
