package pipeline

import (
	"context"
	"net/http"
	"strings"
	"time"

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
	for {
		var f frame
		var ok bool
		select {
		case f, ok = <-frames:
			if !ok {
				// channel 关了却没收到 done：读协程被 ctx 掐断了。
				return p.finish(w, call, encoder, &agg, committed, tail,
					ir.NewError(ir.ErrUpstream, 0, "", "upstream stream ended without a terminator"), rec)
			}
		case <-time.After(timeout):
			kind := "first token"
			if committed {
				kind = "idle"
			}
			return p.finish(w, call, encoder, &agg, committed, tail,
				ir.NewError(ir.ErrTimeout, 0, "", kind+" timeout"), rec)
		}

		if f.err != nil {
			return p.finish(w, call, encoder, &agg, committed, tail, f.err, rec)
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
					writeStreamHeaders(w)
				}
			}
			if ev.Type == ir.EvMessageDelta || ev.Type == ir.EvMessageStop {
				tail = append(tail, ev)
				continue
			}
			if encoder != nil {
				if err := writeEvents(w, encoder, ev); err != nil {
					// 客户端自己断开，上游一直在正常出内容。记成 abnormal 会
					// 累计失败计数、把好目标推向冷却——客户端多按几次停止就能
					// 拖垮账号。按已收 usage 正常记账（上游照样计费）。
					return relayclient.OutcomeNormal, attemptResult{
						err:       ir.NewError(ir.ErrInternal, 0, "", "client went away: "+err.Error()),
						committed: true, usage: usageOf(&agg),
					}
				}
			}
		}

		if f.done {
			// 只在 done 上取解码器说明：读协程此时已送完最后一帧，不会再碰
			// 解码器。超时与断流路径上协程可能正在 Feed，那时读 notes 是数据竞争。
			if n, ok := up.decoder.(codec.StreamNotes); ok {
				rec.addResponseLossy(n.Notes()...)
			}
			return p.finish(w, call, encoder, &agg, committed, tail, streamErr, rec)
		}
	}
}

// finish 收尾：err 为 nil 是成功路径，否则按是否 committed 决定
// 错误落在流内还是当作可换目标的失败回给上层。
func (p *Pipeline) finish(w http.ResponseWriter, call Call, encoder codec.StreamEncoder,
	agg *ir.Aggregator, committed bool, tail []ir.Event, err *ir.Error, rec *Record) (string, attemptResult) {

	usage := usageOf(agg)
	if p.Opts.EstimateUsage && usage.OutputTokens == 0 {
		// 上游没给 usage：按字符数估一个，否则调度层的用量统计会永远是 0。
		resp := agg.Response()
		usage.OutputTokens = int64(ir.EstimateResponse(resp))
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
			writeStreamErrorEnd(w, encoder, err)
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
		p.writeSuccess(w, call, encoder, agg, tail, rec)
		return relayclient.OutcomeNormal, attemptResult{usage: usage}
	}

	if !committed {
		return outcomeFor(err), attemptResult{err: err, usage: usage}
	}

	// committed 之后 HTTP 状态已定：错误只能作为流内事件表达，
	// 且必须补齐未闭合的块，否则客户端会一直等一个不会来的结束帧。
	rec.ErrorCode = string(err.Kind)
	rec.ErrorMessage = err.Message
	if encoder != nil {
		writeStreamErrorEnd(w, encoder, err)
	} else {
		// 非流式客户端：聚合到一半断了，没有半个响应可交，只能回错。
		// 状态码还没写出，所以这里仍能给出正确的 HTTP 错误。
		p.fail(w, call, rec, err)
	}
	return relayclient.OutcomeAbnormal, attemptResult{err: err, committed: true, usage: usage}
}

func (p *Pipeline) writeSuccess(w http.ResponseWriter, call Call, encoder codec.StreamEncoder,
	agg *ir.Aggregator, tail []ir.Event, rec *Record) {
	if encoder != nil {
		// tail 是暂存的上游终止事件，确认这轮完整之后才放行。
		for _, ev := range tail {
			if err := writeEvents(w, encoder, ev); err != nil {
				return
			}
		}
		writeFrames(w, encoder.Finish())
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
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// writeStreamErrorEnd 用流内错误收尾。
//
// 走 encoder 而不是直接调 RenderStreamError：编码器要据此把自己标成
// 已错误终止，Finish 才不会再补一个正常终止帧，把残缺内容伪装成完整回答。
func writeStreamErrorEnd(w http.ResponseWriter, encoder codec.StreamEncoder, err *ir.Error) {
	frames, encErr := encoder.Encode(ir.Event{Type: ir.EvError, Err: err})
	if encErr == nil {
		writeFrames(w, frames)
	}
	writeFrames(w, encoder.Finish())
	flush(w)
}

func writeEvents(w http.ResponseWriter, encoder codec.StreamEncoder, ev ir.Event) error {
	out, err := encoder.Encode(ev)
	if err != nil {
		// 编码失败不该中断整个流：跳过这个事件，其余内容照发。
		return nil
	}
	for _, f := range out {
		if _, err := w.Write(f); err != nil {
			return err
		}
	}
	flush(w)
	return nil
}

func writeFrames(w http.ResponseWriter, frames [][]byte) {
	for _, f := range frames {
		if _, err := w.Write(f); err != nil {
			return
		}
	}
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
