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

// truncatedToolInput 是工具入参被截断时记入流水的错误码。
const truncatedToolInput = "truncated_tool_input"

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
				return p.finish(w, call, encoder, &agg, committed,
					ir.NewError(ir.ErrUpstream, 0, "", "upstream stream ended without a terminator"), rec)
			}
		case <-time.After(timeout):
			kind := "first token"
			if committed {
				kind = "idle"
			}
			return p.finish(w, call, encoder, &agg, committed,
				ir.NewError(ir.ErrTimeout, 0, "", kind+" timeout"), rec)
		}

		if f.err != nil {
			return p.finish(w, call, encoder, &agg, committed, f.err, rec)
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
			if encoder != nil {
				if err := writeEvents(w, encoder, ev); err != nil {
					// 客户端断开：无需上报为上游失败，但这次调用确实没送达。
					return relayclient.OutcomeAbnormal, attemptResult{
						err:       ir.NewError(ir.ErrInternal, 0, "", "client went away: "+err.Error()),
						committed: true, usage: usageOf(&agg),
					}
				}
			}
		}

		if f.done {
			return p.finish(w, call, encoder, &agg, committed, streamErr, rec)
		}
	}
}

// finish 收尾：err 为 nil 是成功路径，否则按是否 committed 决定
// 错误落在流内还是当作可换目标的失败回给上层。
func (p *Pipeline) finish(w http.ResponseWriter, call Call, encoder codec.StreamEncoder,
	agg *ir.Aggregator, committed bool, err *ir.Error, rec *Record) (string, attemptResult) {

	usage := usageOf(agg)
	if p.Opts.EstimateUsage && usage.OutputTokens == 0 {
		// 上游没给 usage：按字符数估一个，否则调度层的用量统计会永远是 0。
		resp := agg.Response()
		usage.OutputTokens = int64(ir.EstimateResponse(resp))
		rec.UsageEstimated = true
	}

	if err == nil {
		if !committed {
			// 流一帧没出就结束了：目标没真正响应，可以换。
			return relayclient.OutcomeRetrying, attemptResult{
				err:   ir.NewError(ir.ErrUpstream, 0, "", "upstream produced no events"),
				usage: usage,
			}
		}
		// 工具入参发到一半断流：残缺调用会被客户端存进历史，
		// 下一轮重放时整个请求都会被上游拒收，不能当成功。
		if truncated := agg.IncompleteTools(); len(truncated) > 0 {
			msg := "truncated tool input: " + strings.Join(truncated, ", ")
			if !call.Stream {
				// 非流式：一个字节都还没写出，可以换目标重来。
				// 这里不写 rec：重试成功后没有代码会把错误字段清掉。
				return relayclient.OutcomeRetrying, attemptResult{
					err:   ir.NewError(ir.ErrUpstream, 0, truncatedToolInput, msg),
					usage: usage,
				}
			}
			// 流式：内容已发出、状态码已定，收不回来了，
			// 只能记下事实并照常终止，至少让客户端拿到一个完整的流。
			rec.ErrorCode = truncatedToolInput
			rec.ErrorMessage = msg
		}
		p.writeSuccess(w, call, encoder, agg, rec)
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
		writeFrames(w, call.Inbound.RenderStreamError(err))
		writeFrames(w, encoder.Finish())
		flush(w)
	} else {
		// 非流式客户端：聚合到一半断了，没有半个响应可交，只能回错。
		// 状态码还没写出，所以这里仍能给出正确的 HTTP 错误。
		p.fail(w, call, rec, err)
	}
	return relayclient.OutcomeAbnormal, attemptResult{err: err, committed: true, usage: usage}
}

func (p *Pipeline) writeSuccess(w http.ResponseWriter, call Call, encoder codec.StreamEncoder,
	agg *ir.Aggregator, rec *Record) {
	if encoder != nil {
		writeFrames(w, encoder.Finish())
		flush(w)
		return
	}
	// 非流式：把聚合结果编成一次性响应。对上游一律流式，
	// 所以这条路径复用同一套解码逻辑，不必单独实现非流式解码。
	body, err := call.Inbound.EncodeResponse(agg.Response())
	if err != nil {
		p.fail(w, call, rec, asIRError(err, ir.ErrInternal))
		return
	}
	rec.StatusCode = http.StatusOK
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
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
